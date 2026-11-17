package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"photography-workflow/agent/internal/copy"
	"photography-workflow/agent/internal/fsx"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/source"
	"photography-workflow/agent/internal/store"
)

type env struct {
	st    *store.Store
	src   string
	a, b  string
	imp   *Importer
	free  uint64
	files map[string]string
}

func newEnv(t *testing.T, twoDests bool) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	e := &env{st: st, src: t.TempDir(), a: t.TempDir(), free: 1 << 40}
	e.files = map[string]string{
		"DCIM/100CANON/IMG_0001.CR3": "raw-bytes",
		"DCIM/100CANON/IMG_0001.JPG": "jpeg-bytes",
	}
	if twoDests {
		e.b = t.TempDir()
	}
	e.writeSource(t)
	e.imp = New(st, Options{
		FreeSpace: func(string) (uint64, error) { return e.free, nil },
	})
	return e
}

func (e *env) writeSource(t *testing.T) {
	t.Helper()
	for rel, body := range e.files {
		full := filepath.Join(e.src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) start(t *testing.T) *model.Import {
	t.Helper()
	src, err := source.NewDir(e.src)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	im, n, err := e.imp.Start(context.Background(), StartRequest{
		ProjectCode: "PRJ-2026-001", ShootDate: "2026-10-29",
		Source: src, DestA: e.a, DestB: e.b, RequireBackup: e.b != "",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if n != len(e.files) {
		t.Fatalf("added = %d, want %d", n, len(e.files))
	}
	return im
}

func TestImportEndToEndDual(t *testing.T) {
	e := newEnv(t, true)
	im := e.start(t)

	res, err := e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Paused || res.Total != 2 || res.LocalVerified != 2 {
		t.Fatalf("result = %+v", res)
	}
	got, err := e.st.Import(context.Background(), im.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ImportLocalComplete {
		t.Fatalf("status = %q", got.Status)
	}
	for _, root := range []string{e.a, e.b} {
		for rel := range e.files {
			if _, err := os.Stat(filepath.Join(root, "PRJ-2026-001", "2026-10-29", filepath.Base(e.src), filepath.FromSlash(rel))); err != nil {
				t.Fatalf("%s on %s: %v", rel, root, err)
			}
		}
	}
	events, err := e.st.UnacknowledgedEvents(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unexpected unacknowledged events: %+v", events)
	}
}

func TestEnrichRunsAfterEveryFileVerified(t *testing.T) {
	e := newEnv(t, true)
	im := e.start(t)
	calls, verifiedAtCall := 0, -1
	e.imp = New(e.st, Options{
		FreeSpace: func(string) (uint64, error) { return e.free, nil },
		Enrich: func(ctx context.Context, got *model.Import) error {
			calls++
			if got.ID != im.ID {
				t.Errorf("enrich import = %s, want %s", got.ID, im.ID)
			}
			counts, err := e.st.FileCounts(ctx, got.ID)
			if err != nil {
				return err
			}
			verifiedAtCall = counts.LocalVerified
			return nil
		},
	})
	res, err := e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Paused || res.LocalVerified != len(e.files) {
		t.Fatalf("result = %+v", res)
	}
	if calls != 1 || verifiedAtCall != len(e.files) {
		t.Fatalf("enrich calls = %d verified = %d, want 1/%d", calls, verifiedAtCall, len(e.files))
	}
}

func TestEnrichFailureOnlyWarns(t *testing.T) {
	e := newEnv(t, false)
	im := e.start(t)
	e.imp = New(e.st, Options{
		FreeSpace: func(string) (uint64, error) { return e.free, nil },
		Enrich: func(context.Context, *model.Import) error {
			return errors.New("boom")
		},
	})
	res, err := e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Paused || res.LocalVerified != len(e.files) {
		t.Fatalf("result = %+v", res)
	}
	got, err := e.st.Import(context.Background(), im.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ImportLocalComplete {
		t.Fatalf("status = %q, enrich must not block completion", got.Status)
	}
	events, err := e.st.UnacknowledgedEvents(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Code == "metadata_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no metadata_failed warning: %+v", events)
	}
}

func TestStartRejectsDuplicateCard(t *testing.T) {
	e := newEnv(t, false)
	im := e.start(t)
	src, _ := source.NewDir(e.src)
	existing, _, err := e.imp.Start(context.Background(), StartRequest{
		ProjectCode: "PRJ-2026-001", ShootDate: "2026-10-29",
		Source: src, DestA: e.a,
	})
	if !errors.Is(err, ErrImportExists) {
		t.Fatalf("err = %v, want ErrImportExists", err)
	}
	if existing == nil || existing.ID != im.ID {
		t.Fatalf("existing import = %+v", existing)
	}
	// Explicit AllowDuplicate is the operator's "import again" choice.
	if _, _, err := e.imp.Start(context.Background(), StartRequest{
		ProjectCode: "PRJ-2026-001", ShootDate: "2026-10-29",
		Source: src, DestA: e.a, AllowDuplicate: true,
	}); err != nil {
		t.Fatalf("allow duplicate: %v", err)
	}
}

func TestRunPausesWhenCardRemovedThenResumes(t *testing.T) {
	e := newEnv(t, true)
	im := e.start(t)
	if err := os.RemoveAll(e.src); err != nil {
		t.Fatal(err)
	}
	res, err := e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Reason != string(fsx.ReasonCardRemoved) {
		t.Fatalf("result = %+v", res)
	}
	got, _ := e.st.Import(context.Background(), im.ID)
	if got.PauseReason != string(fsx.ReasonCardRemoved) {
		t.Fatalf("pause reason = %q", got.PauseReason)
	}
	unacked, _ := e.st.UnacknowledgedEvents(context.Background(), 10)
	if len(unacked) != 1 || unacked[0].Code != "import_paused" {
		t.Fatalf("events = %+v", unacked)
	}

	e.writeSource(t)
	res, err = e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Paused || res.LocalVerified != 2 {
		t.Fatalf("resume result = %+v", res)
	}
	got, _ = e.st.Import(context.Background(), im.ID)
	if got.PauseReason != "" || got.Status != model.ImportLocalComplete {
		t.Fatalf("import = %+v", got)
	}
}

func TestRunPausesOnDiskFull(t *testing.T) {
	e := newEnv(t, true)
	im := e.start(t)
	e.free = 100
	res, err := e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Reason != string(fsx.ReasonDiskFull) {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(e.a, "PRJ-2026-001")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no file may be written while disk-full pause is detected")
	}
	got, _ := e.st.Import(context.Background(), im.ID)
	if got.Status != model.ImportOpen {
		t.Fatalf("status = %q", got.Status)
	}

	e.free = 1 << 40
	res, err = e.imp.Run(context.Background(), im.ID)
	if err != nil || res.Paused || res.LocalVerified != 2 {
		t.Fatalf("resume: %+v %v", res, err)
	}
}

func TestRunRecoversInterruptedState(t *testing.T) {
	e := newEnv(t, false)
	im := e.start(t)
	files, err := e.st.Files(context.Background(), im.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := files[0]
	card, err := e.st.Card(context.Background(), im.CardID)
	if err != nil {
		t.Fatal(err)
	}
	layout := copy.Layout{ProjectCode: im.ProjectCode, ShootDate: im.ShootDate, CardLabel: card.Label}
	final, err := layout.Merge(e.a, f.RelPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsx.EnsureDir(filepath.Dir(final)); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(filepath.Dir(final), ".pwf-crash-dead.part")
	if err := os.WriteFile(temp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SaveCopy(context.Background(), model.Copy{
		FileID: f.ID, Dest: model.DestA, FinalPath: final, TempPath: temp, State: model.CopyCopying,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.st.SetFileState(context.Background(), f.ID,
		[]model.FileState{model.StateDiscovered}, model.StateCopying, ""); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}

	res, err := e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.LocalVerified != 2 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan temp not cleaned: %v", err)
	}
}

func TestStatusReportsCountsAndAlerts(t *testing.T) {
	e := newEnv(t, false)
	im := e.start(t)
	if _, err := e.imp.Run(context.Background(), im.ID); err != nil {
		t.Fatal(err)
	}
	st, err := e.imp.Status(context.Background(), im.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Counts.Total != 2 || st.Counts.LocalVerified != 2 {
		t.Fatalf("counts = %+v", st.Counts)
	}
	if st.Import.ID != im.ID {
		t.Fatalf("import id = %q", st.Import.ID)
	}
}

func TestStartValidation(t *testing.T) {
	e := newEnv(t, false)
	src, _ := source.NewDir(e.src)
	cases := []struct {
		name string
		req  StartRequest
	}{
		{"no project code", StartRequest{ShootDate: "2026-10-29", Source: src, DestA: e.a}},
		{"bad date", StartRequest{ProjectCode: "P", ShootDate: "29/10/2026", Source: src, DestA: e.a}},
		{"relative dest", StartRequest{ProjectCode: "P", ShootDate: "2026-10-29", Source: src, DestA: "relative"}},
		{"same dests", StartRequest{ProjectCode: "P", ShootDate: "2026-10-29", Source: src, DestA: e.a, DestB: e.a, RequireBackup: true}},
		{"dest inside card", StartRequest{ProjectCode: "P", ShootDate: "2026-10-29", Source: src, DestA: filepath.Join(e.src, "out")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := e.imp.Start(context.Background(), c.req); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestStartRejectsEmptySource(t *testing.T) {
	e := newEnv(t, false)
	if err := os.RemoveAll(e.src); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.src, 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := source.NewDir(e.src)
	_, _, err := e.imp.Start(context.Background(), StartRequest{
		ProjectCode: "P", ShootDate: "2026-10-29", Source: src, DestA: e.a,
	})
	if !errors.Is(err, ErrEmptySource) {
		t.Fatalf("err = %v", err)
	}
}

func TestManualCardStable(t *testing.T) {
	a := ManualCard(t.TempDir())
	b := ManualCard(".")
	if a.Fingerprint == "" || b.Fingerprint == "" {
		t.Fatal("fingerprint missing")
	}
	if a.Fingerprint == b.Fingerprint {
		t.Fatal("different roots must not share a fingerprint")
	}
	dir := t.TempDir()
	if ManualCard(dir).Fingerprint != ManualCard(dir).Fingerprint {
		t.Fatal("fingerprint must be stable for the same root")
	}
}
