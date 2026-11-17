package copy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"photography-workflow/agent/internal/fsx"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/source"
	"photography-workflow/agent/internal/store"
)

var testLayout = Layout{ProjectCode: "PRJ-2026-001", ShootDate: "2026-10-29", CardLabel: "CARD_A"}

type fixture struct {
	st    *store.Store
	src   *source.Dir
	srcIn string
	a, b  string
	imp   model.Import
	files []model.File
}

func setup(t *testing.T, contents map[string]string, dests int) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srcRoot := t.TempDir()
	for rel, body := range contents {
		full := filepath.Join(srcRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := source.NewDir(srcRoot)
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	now := time.Now().UTC()
	card, err := st.UpsertCard(ctx, model.Card{
		ID: uuid.NewString(), Fingerprint: "fp-" + uuid.NewString(),
		Label: "CARD_A", FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	a := t.TempDir()
	b := ""
	if dests > 1 {
		b = t.TempDir()
	}
	imp := model.Import{
		ID: uuid.NewString(), ProjectCode: testLayout.ProjectCode,
		CardID: card.ID, SourceRoot: srcRoot, ShootDate: testLayout.ShootDate,
		DestA: a, DestB: b, RequireBackup: dests > 1,
		Status: model.ImportOpen, CreatedAt: now,
	}
	if err := st.CreateImport(ctx, imp); err != nil {
		t.Fatalf("import: %v", err)
	}

	var files []model.File
	for rel, body := range contents {
		files = append(files, model.File{
			ID: uuid.NewString(), ImportID: imp.ID, RelPath: rel,
			Size: int64(len(body)), MtimeNS: now.UnixNano(),
			Kind: model.KindPhoto, Role: model.RoleRAW, State: model.StateDiscovered,
		})
	}
	if _, err := st.InsertFiles(ctx, files); err != nil {
		t.Fatalf("files: %v", err)
	}
	stored, err := st.Files(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload files: %v", err)
	}
	return &fixture{st: st, src: src, srcIn: srcRoot, a: a, b: b, imp: imp, files: stored}
}

func (fx *fixture) engine(dests int) *Engine {
	e := &Engine{Store: fx.st, Dests: []Dest{{Name: model.DestA, Root: fx.a}}}
	if dests > 1 {
		e.Dests = append(e.Dests, Dest{Name: model.DestB, Root: fx.b})
	}
	return e
}

func (fx *fixture) run(t *testing.T, dests int) {
	t.Helper()
	e := fx.engine(dests)
	for _, f := range fx.files {
		if err := e.RunFile(context.Background(), fx.src, f, testLayout); err != nil {
			t.Fatalf("copy %s: %v", f.RelPath, err)
		}
	}
}

func (fx *fixture) final(t *testing.T, root, rel string) string {
	t.Helper()
	p, err := testLayout.Merge(root, rel)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func assertNoTemps(t *testing.T, roots ...string) {
	t.Helper()
	for _, root := range roots {
		if root == "" {
			continue
		}
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasPrefix(info.Name(), ".pwf-") {
				t.Fatalf("leftover temporary file: %s", p)
			}
			return nil
		})
	}
}

func TestDualCopyVerifiesBothSides(t *testing.T) {
	fx := setup(t, map[string]string{
		"DCIM/100CANON/IMG_0001.CR3": "raw-bytes",
		"DCIM/100CANON/IMG_0001.JPG": "jpeg-bytes",
	}, 2)
	fx.run(t, 2)

	for _, f := range fx.files {
		got, err := fx.st.File(context.Background(), f.ID)
		if err != nil {
			t.Fatalf("load file: %v", err)
		}
		if got.State != model.StateLocalVerified {
			t.Fatalf("%s state = %q", f.RelPath, got.State)
		}
		if len(got.SHA256) != 64 {
			t.Fatalf("%s sha = %q", f.RelPath, got.SHA256)
		}
		for _, root := range []string{fx.a, fx.b} {
			body, err := os.ReadFile(fx.final(t, root, f.RelPath))
			if err != nil {
				t.Fatalf("%s on %s: %v", f.RelPath, root, err)
			}
			if len(body) != int(f.Size) {
				t.Fatalf("%s size = %d", f.RelPath, len(body))
			}
		}
		copies, err := fx.st.Copies(context.Background(), f.ID)
		if err != nil {
			t.Fatalf("copies: %v", err)
		}
		if len(copies) != 2 {
			t.Fatalf("copies = %d", len(copies))
		}
		for _, c := range copies {
			if c.State != model.CopyVerified || c.SHA256 != got.SHA256 {
				t.Fatalf("copy %s = %+v", c.Dest, c)
			}
		}
	}
	assertNoTemps(t, fx.a, fx.b)
}

func TestSingleDestination(t *testing.T) {
	fx := setup(t, map[string]string{"a.txt": "one"}, 1)
	fx.run(t, 1)
	copies, err := fx.st.Copies(context.Background(), fx.files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].Dest != model.DestA || copies[0].State != model.CopyVerified {
		t.Fatalf("copies = %+v", copies)
	}
}

func TestAdoptsExistingMatchingFinal(t *testing.T) {
	fx := setup(t, map[string]string{"DCIM/IMG_0001.CR3": "same-bytes"}, 2)
	finalA := fx.final(t, fx.a, "DCIM/IMG_0001.CR3")
	if err := fsx.EnsureDir(filepath.Dir(finalA)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalA, []byte("same-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.run(t, 2)

	got, _ := fx.st.File(context.Background(), fx.files[0].ID)
	if got.State != model.StateLocalVerified {
		t.Fatalf("state = %q", got.State)
	}
	if _, err := os.Stat(fx.final(t, fx.a, "DCIM/IMG_0001__2.CR3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("matching final should be adopted, not duplicated")
	}
	copies, _ := fx.st.Copies(context.Background(), fx.files[0].ID)
	for _, c := range copies {
		if c.Dest == model.DestA && !fsx.SamePath(c.FinalPath, finalA) {
			t.Fatalf("A final = %q, want %q", c.FinalPath, finalA)
		}
	}
	assertNoTemps(t, fx.a, fx.b)
}

func TestConflictingFinalGetsAltName(t *testing.T) {
	fx := setup(t, map[string]string{"DCIM/IMG_0001.CR3": "new-bytes"}, 1)
	finalA := fx.final(t, fx.a, "DCIM/IMG_0001.CR3")
	if err := fsx.EnsureDir(filepath.Dir(finalA)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalA, []byte("old-unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.run(t, 1)

	if b, _ := os.ReadFile(finalA); string(b) != "old-unrelated" {
		t.Fatalf("existing file was modified: %q", b)
	}
	alt := fx.final(t, fx.a, "DCIM/IMG_0001__2.CR3")
	if b, err := os.ReadFile(alt); err != nil || string(b) != "new-bytes" {
		t.Fatalf("alt = %q, %v", b, err)
	}
	copies, _ := fx.st.Copies(context.Background(), fx.files[0].ID)
	if len(copies) != 1 || !fsx.SamePath(copies[0].FinalPath, alt) || copies[0].State != model.CopyVerified {
		t.Fatalf("copies = %+v", copies)
	}
}

func TestResumeAdoptsRecordedCopy(t *testing.T) {
	fx := setup(t, map[string]string{"DCIM/IMG_0002.CR3": "resume-bytes"}, 1)
	finalA := fx.final(t, fx.a, "DCIM/IMG_0002.CR3")
	if err := fsx.EnsureDir(filepath.Dir(finalA)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalA, []byte("resume-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A crash after the rename but before the DB update leaves a pending row
	// with a recorded final path.
	if err := fx.st.SaveCopy(context.Background(), model.Copy{
		FileID: fx.files[0].ID, Dest: model.DestA, FinalPath: finalA,
		State: model.CopyCopied, Bytes: 12,
	}); err != nil {
		t.Fatal(err)
	}
	fx.run(t, 1)
	got, _ := fx.st.File(context.Background(), fx.files[0].ID)
	if got.State != model.StateLocalVerified {
		t.Fatalf("state = %q", got.State)
	}
	if _, err := os.Stat(fx.final(t, fx.a, "DCIM/IMG_0002__2.CR3")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resume duplicated the file")
	}
}

func TestMissingSourceFailsFile(t *testing.T) {
	fx := setup(t, map[string]string{"gone.txt": "x"}, 1)
	if err := os.Remove(filepath.Join(fx.srcIn, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	err := fx.engine(1).RunFile(context.Background(), fx.src, fx.files[0], testLayout)
	if err == nil {
		t.Fatal("expected failure for missing source")
	}
	var stop *StopError
	if errors.As(err, &stop) {
		t.Fatalf("missing single file must not pause import: %v", err)
	}
	got, _ := fx.st.File(context.Background(), fx.files[0].ID)
	if got.State != model.StateCopyFailed {
		t.Fatalf("state = %q", got.State)
	}
	copies, _ := fx.st.Copies(context.Background(), fx.files[0].ID)
	if len(copies) == 0 {
		t.Fatal("failed copy row missing")
	}
	for _, c := range copies {
		if c.State != model.CopyFailed {
			t.Fatalf("copy state = %q", c.State)
		}
	}
	assertNoTemps(t, fx.a)
}

func TestRemovedSourcePauses(t *testing.T) {
	fx := setup(t, map[string]string{"a.txt": "x"}, 1)
	if err := os.RemoveAll(fx.srcIn); err != nil {
		t.Fatal(err)
	}
	err := fx.engine(1).RunFile(context.Background(), fx.src, fx.files[0], testLayout)
	var stop *StopError
	if !errors.As(err, &stop) || stop.Reason != fsx.ReasonCardRemoved {
		t.Fatalf("err = %v, want card_removed stop", err)
	}
}

func TestLayoutRejectsEscape(t *testing.T) {
	if _, err := testLayout.Merge(t.TempDir(), "../evil"); err == nil {
		t.Fatal("escape should be rejected")
	}
	root := t.TempDir()
	p, err := testLayout.Merge(root, "DCIM/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !fsx.Within(root, p) {
		t.Fatalf("merged path escapes: %s", p)
	}
	if !strings.Contains(p, filepath.Join("PRJ-2026-001", "2026-10-29", "CARD_A", "DCIM")) {
		t.Fatalf("unexpected layout: %s", p)
	}
}
