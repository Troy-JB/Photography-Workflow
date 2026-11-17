// Package selftest exercises the local import stack end to end with scratch
// data: migrations, dual-destination copy, pause/resume and restart recovery.
// The Windows CI job and `pwf-agent selftest` both use it.
package selftest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"photography-workflow/agent/internal/core"
	"photography-workflow/agent/internal/fsx"
	contenthash "photography-workflow/agent/internal/hash"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/source"
	"photography-workflow/agent/internal/store"
)

type Step struct {
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

type Report struct {
	OK    bool   `json:"ok"`
	Steps []Step `json:"steps"`
}

func Run(ctx context.Context) Report {
	var report Report
	scratch, err := os.MkdirTemp("", "pwf-selftest-*")
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "scratch", Detail: err.Error()})
		return report
	}
	defer os.RemoveAll(scratch)

	report.Steps = append(report.Steps, runStep("store migrations up/down", func() error {
		dbPath := filepath.Join(scratch, "migrations.db")
		st, err := store.Open(ctx, dbPath)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.DownTo(ctx, 0); err != nil {
			return err
		}
		return st.Up(ctx)
	}))

	dbPath := filepath.Join(scratch, "state.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "open store", Detail: err.Error()})
		return report
	}
	defer func() { _ = st.Close() }()

	srcDir := filepath.Join(scratch, "card")
	files := map[string][]byte{
		"DCIM/100CANON/IMG_0001.CR3": patternBytes(2<<20 + 12345),
		"DCIM/100CANON/IMG_0001.JPG": []byte("jpeg-side"),
	}
	if err := writeTree(srcDir, files); err != nil {
		report.Steps = append(report.Steps, Step{Name: "fixture", Detail: err.Error()})
		return report
	}
	// OS noise must be ignored by the scanner.
	if err := os.WriteFile(filepath.Join(srcDir, "DCIM", "Thumbs.db"), []byte("junk"), 0o600); err != nil {
		report.Steps = append(report.Steps, Step{Name: "fixture", Detail: err.Error()})
		return report
	}

	destA := filepath.Join(scratch, "archive-a")
	destB := filepath.Join(scratch, "archive-b")
	for _, d := range []string{destA, destB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			report.Steps = append(report.Steps, Step{Name: "fixture", Detail: err.Error()})
			return report
		}
	}

	imp := core.New(st, core.Options{Workers: 2})
	src, err := source.NewDir(srcDir)
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "source", Detail: err.Error()})
		return report
	}
	im, added, err := imp.Start(ctx, core.StartRequest{
		ProjectCode: "SELFTEST", ShootDate: time.Now().UTC().Format("2006-01-02"),
		Source: src, DestA: destA, DestB: destB, RequireBackup: true,
	})
	if err != nil {
		report.Steps = append(report.Steps, Step{Name: "start import", Detail: err.Error()})
		return report
	}

	report.Steps = append(report.Steps, runStep("dual copy + three-way hash", func() error {
		res, err := imp.Run(ctx, im.ID)
		if err != nil {
			return err
		}
		if res.Paused || res.LocalVerified != added || added != len(files) {
			return fmt.Errorf("result = %+v (added %d)", res, added)
		}
		for rel, body := range files {
			want := shaOf(body)
			for _, root := range []string{destA, destB} {
				final := filepath.Join(root, "SELFTEST", im.ShootDate, "card", filepath.FromSlash(rel))
				got, err := contenthash.File(ctx, final)
				if err != nil {
					return fmt.Errorf("%s: %w", final, err)
				}
				if got.SHA256 != want {
					return fmt.Errorf("%s hash %s != %s", final, got.SHA256, want)
				}
			}
		}
		status, err := imp.Status(ctx, im.ID)
		if err != nil {
			return err
		}
		if status.Import.Status != model.ImportLocalComplete {
			return fmt.Errorf("status = %q", status.Import.Status)
		}
		return nil
	}))

	report.Steps = append(report.Steps, runStep("card removal pause + resume", func() error {
		pauseSrc := filepath.Join(scratch, "card2")
		if err := writeTree(pauseSrc, map[string][]byte{"DCIM/B.CR3": []byte("resume-me")}); err != nil {
			return err
		}
		s2, err := source.NewDir(pauseSrc)
		if err != nil {
			return err
		}
		im2, _, err := imp.Start(ctx, core.StartRequest{
			ProjectCode: "SELFTEST-2", ShootDate: im.ShootDate,
			Source: s2, DestA: destA, DestB: destB, RequireBackup: true,
		})
		if err != nil {
			return err
		}
		if err := os.RemoveAll(pauseSrc); err != nil {
			return err
		}
		res, err := imp.Run(ctx, im2.ID)
		if err != nil {
			return err
		}
		if !res.Paused || res.Reason != "card_removed" {
			return fmt.Errorf("expected card_removed pause, got %+v", res)
		}
		if err := writeTree(pauseSrc, map[string][]byte{"DCIM/B.CR3": []byte("resume-me")}); err != nil {
			return err
		}
		res, err = imp.Run(ctx, im2.ID)
		if err != nil {
			return err
		}
		if res.Paused || res.LocalVerified != 1 {
			return fmt.Errorf("resume result = %+v", res)
		}
		return nil
	}))

	report.Steps = append(report.Steps, runStep("restart keeps state", func() error {
		if err := st.Close(); err != nil {
			return err
		}
		st, err = store.Open(ctx, dbPath)
		if err != nil {
			return err
		}
		imp = core.New(st, core.Options{Workers: 2})
		status, err := imp.Status(ctx, im.ID)
		if err != nil {
			return err
		}
		if status.Import.Status != model.ImportLocalComplete || status.Counts.LocalVerified != added {
			return fmt.Errorf("status = %+v", status)
		}
		return nil
	}))

	report.OK = true
	for _, s := range report.Steps {
		if !s.OK {
			report.OK = false
		}
	}
	return report
}

func runStep(name string, fn func() error) Step {
	start := time.Now()
	err := fn()
	s := Step{Name: name, OK: err == nil, DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		s.Detail = err.Error()
	}
	return s
}

func writeTree(root string, files map[string][]byte) error {
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := fsx.EnsureDir(filepath.Dir(full)); err != nil {
			return err
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

func shaOf(body []byte) string {
	h := contenthash.New()
	_, _ = h.Write(body)
	return h.SHA256()
}
