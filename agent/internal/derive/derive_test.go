package derive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/preview"
	"photography-workflow/agent/internal/store"
)

type env struct {
	d       *Derive
	st      *store.Store
	imp     model.Import
	workDir string
}

// newEnv seeds one import. verified writes a file into dir and records a
// verified copy for it, which is what Enrich and Preview read from.
func newEnv(t *testing.T, exifPath, ffmpeg string) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	card, err := st.UpsertCard(ctx, model.Card{
		ID: "card-1", Fingerprint: "fp-1", Label: "CARD_A",
		Capacity: 1 << 30, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	imp := model.Import{
		ID: "imp-1", ProjectCode: "PRJ-2026-001", CardID: card.ID,
		SourceRoot: "/cards/CARD_A", ShootDate: "2026-10-29",
		DestA: filepath.Join(dir, "a"), DestB: filepath.Join(dir, "b"),
		RequireBackup: true, Status: model.ImportOpen, CreatedAt: now,
	}
	if err := st.CreateImport(ctx, imp); err != nil {
		t.Fatalf("import: %v", err)
	}

	d := New(st, filepath.Join(dir, "cache"), exifPath, ffmpeg, nil)
	t.Cleanup(func() { _ = d.Close() })
	return &env{d: d, st: st, imp: imp, workDir: dir}
}

func (e *env) addFile(t *testing.T, rel string, kind model.FileKind, role model.FileRole) model.File {
	t.Helper()
	f := model.File{
		ID: "file-" + strings.ReplaceAll(rel, "/", "_"), ImportID: e.imp.ID,
		RelPath: rel, Size: 1024, MtimeNS: time.Now().UnixNano(),
		Kind: kind, Role: role, State: model.StateDiscovered,
	}
	if _, err := e.st.InsertFiles(context.Background(), []model.File{f}); err != nil {
		t.Fatalf("insert %s: %v", rel, err)
	}
	return f
}

// verify writes content to dest/<rel> and records a verified copy.
func (e *env) verify(t *testing.T, f model.File, dest string, content []byte) string {
	t.Helper()
	path := filepath.Join(e.workDir, dest, filepath.FromSlash(f.RelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	if err := e.st.SaveCopy(context.Background(), model.Copy{
		FileID: f.ID, Dest: dest, FinalPath: path, State: model.CopyVerified,
		Bytes: int64(len(content)), SHA256: sha,
	}); err != nil {
		t.Fatalf("save copy: %v", err)
	}
	if _, err := e.st.FinishLocalMove(context.Background(), f.ID,
		[]model.FileState{model.StateDiscovered}, model.StateLocalVerified, sha, ""); err != nil {
		t.Fatalf("finish move: %v", err)
	}
	return sha
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func buildFake(t *testing.T, pkg string) string {
	t.Helper()
	name := filepath.Base(pkg)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, pkg)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, combined)
	}
	return out
}

func (e *env) events(t *testing.T, code string) []model.Event {
	t.Helper()
	all, err := e.st.UnacknowledgedEvents(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Event
	for _, ev := range all {
		if code == "" || ev.Code == code {
			out = append(out, ev)
		}
	}
	return out
}

func TestEnrichStoresMetadata(t *testing.T) {
	exif := buildFake(t, "photography-workflow/agent/internal/meta/testdata/fakeexiftool")
	e := newEnv(t, exif, "")

	photo := e.addFile(t, "DCIM/100/IMG_0001.JPG", model.KindPhoto, model.RoleJPEG)
	e.verify(t, photo, model.DestA, jpegBytes(t, 64, 48))
	sidecar := e.addFile(t, "DCIM/100/IMG_0001.XMP", model.KindSidecar, model.RoleSidecar)
	e.verify(t, sidecar, model.DestA, []byte("<xmp/>"))

	if err := e.d.Enrich(context.Background(), &e.imp); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	got, err := e.st.File(context.Background(), photo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Camera != "Canon EOS R5" || got.Width != 6000 || got.Height != 4000 {
		t.Fatalf("metadata = %+v", got)
	}
	if got.Mime != "image/jpeg" {
		t.Fatalf("mime = %q", got.Mime)
	}
	if got.ExifJSON == "" || len(got.ExifJSON) > 4096 {
		t.Fatalf("exif digest = %d bytes", len(got.ExifJSON))
	}
	sc, err := e.st.File(context.Background(), sidecar.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sc.ExifJSON != "" {
		t.Fatalf("sidecar gained metadata: %+v", sc)
	}
	if evs := e.events(t, ""); len(evs) != 0 {
		t.Fatalf("unexpected events: %+v", evs)
	}
}

func TestEnrichWithoutExifToolWarnsOnce(t *testing.T) {
	e := newEnv(t, "", "")
	photo := e.addFile(t, "DCIM/IMG_0001.JPG", model.KindPhoto, model.RoleJPEG)
	e.verify(t, photo, model.DestA, jpegBytes(t, 32, 32))

	if err := e.d.Enrich(context.Background(), &e.imp); err != nil {
		t.Fatalf("enrich must not fail: %v", err)
	}
	evs := e.events(t, "metadata_unavailable")
	if len(evs) != 1 {
		t.Fatalf("metadata_unavailable events = %d", len(evs))
	}
	got, err := e.st.File(context.Background(), photo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExifJSON != "" {
		t.Fatalf("file gained metadata without a tool: %+v", got)
	}
}

func TestEnrichSkipsFilesWithMetadata(t *testing.T) {
	e := newEnv(t, filepath.Join(t.TempDir(), "missing-exiftool"), "")
	photo := e.addFile(t, "DCIM/IMG_0001.JPG", model.KindPhoto, model.RoleJPEG)
	if err := e.st.SetFileMetadata(context.Background(), photo.ID,
		model.FileMetadata{Camera: "already there", ExifJSON: `{"EXIF:Make":"Canon"}`}); err != nil {
		t.Fatal(err)
	}
	e.verify(t, photo, model.DestA, jpegBytes(t, 32, 32))

	if err := e.d.Enrich(context.Background(), &e.imp); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if evs := e.events(t, ""); len(evs) != 0 {
		t.Fatalf("a broken tool must not be touched when nothing is pending: %+v", evs)
	}
}

func TestPreviewJPEGCachedAndFallbackB(t *testing.T) {
	e := newEnv(t, "", "")
	photo := e.addFile(t, "DCIM/IMG_0001.JPG", model.KindPhoto, model.RoleJPEG)
	sha := e.verify(t, photo, model.DestB, jpegBytes(t, 1200, 800))
	_ = sha
	f, err := e.st.File(context.Background(), photo.ID)
	if err != nil {
		t.Fatal(err)
	}

	path, err := e.d.Preview(context.Background(), *f)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if want := filepath.Join(e.workDir, "cache", f.SHA256, "512.jpg"); path != want {
		t.Fatalf("path = %s, want %s", path, want)
	}
	img, err := jpeg.Decode(mustOpen(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 341 {
		t.Fatalf("preview = %dx%d", b.Dx(), b.Dy())
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	again, err := e.d.Preview(context.Background(), *f)
	if err != nil || again != path {
		t.Fatalf("second preview = %s, %v", again, err)
	}
	after, err := os.Stat(again)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("cached preview was regenerated")
	}
}

func TestPreviewWithoutVerifiedCopyFails(t *testing.T) {
	e := newEnv(t, "", "")
	photo := e.addFile(t, "DCIM/IMG_0001.JPG", model.KindPhoto, model.RoleJPEG)
	f, err := e.st.File(context.Background(), photo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Preview(context.Background(), *f); err == nil {
		t.Fatal("preview without a verified copy must fail")
	}
}

func TestPreviewSidecarIsUnsupportedAndSilent(t *testing.T) {
	e := newEnv(t, "", "")
	sc := e.addFile(t, "DCIM/IMG_0001.XMP", model.KindSidecar, model.RoleSidecar)
	f, err := e.st.File(context.Background(), sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Preview(context.Background(), *f); !errors.Is(err, preview.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if evs := e.events(t, ""); len(evs) != 0 {
		t.Fatalf("sidecar preview must not emit events: %+v", evs)
	}
}

func TestPreviewFailureEmitsEvent(t *testing.T) {
	e := newEnv(t, "", filepath.Join(t.TempDir(), "missing-ffmpeg"))
	video := e.addFile(t, "DCIM/CLIP.MP4", model.KindVideo, model.RoleVideo)
	e.verify(t, video, model.DestA, []byte("not a video"))
	f, err := e.st.File(context.Background(), video.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Preview(context.Background(), *f); err == nil {
		t.Fatal("preview without ffmpeg must fail")
	}
	if evs := e.events(t, "derivative_failed"); len(evs) != 1 {
		t.Fatalf("derivative_failed events = %d", len(evs))
	}
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
