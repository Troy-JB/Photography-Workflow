package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/storage"
)

func TestNewProviderValidatesChannel(t *testing.T) {
	for _, name := range []string{ProviderFTP, ProviderPixCake, ProviderXiangxiang} {
		if _, err := New(Config{Provider: name}, nil); !errors.Is(err, ErrProviderNotImplemented) {
			t.Errorf("New(%q) err = %v, want ErrProviderNotImplemented", name, err)
		}
	}
	if _, err := New(Config{Provider: ProviderFolderWatch}, nil); !errors.Is(err, ErrProviderNotImplemented) {
		t.Errorf("folder_watch without root should fail, got %v", err)
	}
	if _, err := New(Config{Provider: ProviderFolderWatch, Root: t.TempDir()}, memStore{}); err != nil {
		t.Errorf("folder_watch with root: %v", err)
	}
	p, err := New(Config{}, nil)
	if err != nil || p.Name() != ProviderManual {
		t.Errorf("default provider = %v, %v; want manual", p, err)
	}
	if ref, err := p.Export(context.Background(), Task{}); err != nil || ref != "" {
		t.Errorf("manual export = %q, %v; want no-op", ref, err)
	}
}

func TestFolderWatchExportAndPoll(t *testing.T) {
	root := t.TempDir()
	store := memStore{
		"t/t1/originals/a.raw": []byte("raw-a"),
		"t/t1/originals/b.jpg": []byte("jpg-b"),
	}
	fw := NewFolderWatch(root, store)
	task := Task{
		ID:        uuid.New(),
		TenantID:  uuid.New(),
		RoundID:   uuid.New(),
		RoundNo:   3,
		StartedAt: time.Now(),
		Items: []TaskItem{
			{AssetID: uuid.New(), Filename: "IMG_0001.CR3", Position: 1, Ext: ".CR3", SourceKey: "t/t1/originals/a.raw"},
			{AssetID: uuid.New(), Filename: "IMG_0002.JPG", Position: 2, Ext: ".JPG", SourceKey: "t/t1/originals/b.jpg"},
		},
	}
	ref, err := fw.Export(context.Background(), task)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if want := filepath.ToSlash(filepath.Join(folderOutgoing, task.ID.String())); ref != want {
		t.Errorf("ref = %q, want %q", ref, want)
	}
	base := filepath.Join(root, folderOutgoing, task.ID.String())
	raw, err := os.ReadFile(filepath.Join(base, manifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest folderManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Format != manifestFormat || manifest.RoundNo != 3 || len(manifest.Items) != 2 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	src := filepath.Join(base, folderSource, task.Items[1].AssetID.String()+".jpg")
	if got, err := os.ReadFile(src); err != nil || string(got) != "jpg-b" {
		t.Errorf("source copy = %q, %v", got, err)
	}
	// Re-export must be idempotent (overwrites the same files).
	if _, err := fw.Export(context.Background(), task); err != nil {
		t.Fatalf("re-export: %v", err)
	}

	dir := filepath.Join(root, folderIncoming, task.ID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	retouched := makeJPEG(t, 32, 24)
	good := filepath.Join(dir, task.Items[0].AssetID.String()+".jpg")
	if err := os.WriteFile(good, retouched, 0o644); err != nil {
		t.Fatal(err)
	}
	// Names that are not asset ids or not images are ignored.
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".partial.jpg"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, folderDone), 0o755)
	_ = os.WriteFile(filepath.Join(dir, folderDone, task.Items[1].AssetID.String()+".jpg"), []byte("old"), 0o644)

	files, err := fw.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("poll returned %d files, want 1", len(files))
	}
	if files[0].TaskID != task.ID || files[0].AssetID != task.Items[0].AssetID || files[0].Size != int64(len(retouched)) {
		t.Errorf("unexpected import: %+v", files[0])
	}
	if err := fw.Archive(files[0], true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, folderDone, task.Items[0].AssetID.String()+".jpg")); err != nil {
		t.Errorf("archived file missing: %v", err)
	}
	again, err := fw.Poll(context.Background())
	if err != nil {
		t.Fatalf("poll after archive: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("archived file was polled again: %+v", again)
	}
}

func TestFolderWatchRejectsEscapingArchive(t *testing.T) {
	fw := NewFolderWatch(t.TempDir(), memStore{})
	outside := filepath.Join(t.TempDir(), "evil.jpg")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fw.Archive(Import{Path: outside}, true); err == nil {
		t.Fatal("archive outside the root should fail")
	}
}

func TestInspectImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rev.jpg")
	if err := os.WriteFile(path, makeJPEG(t, 40, 30), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	sha, w, h, err := inspectImage(f, info.Size(), 1<<20)
	if err != nil {
		t.Fatalf("inspectImage: %v", err)
	}
	if len(sha) != 64 || w != 40 || h != 30 {
		t.Errorf("sha=%q w=%d h=%d", sha, w, h)
	}

	notImage := filepath.Join(dir, "rev.txt")
	if err := os.WriteFile(notImage, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := os.Open(notImage)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if _, _, _, err := inspectImage(bad, 11, 1<<20); err == nil {
		t.Error("inspectImage accepted a text file")
	}
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 6), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

type memStore map[string][]byte

func (m memStore) EnsureBucket(context.Context) error { return nil }
func (m memStore) Health(context.Context) error       { return nil }
func (m memStore) PresignPut(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (m memStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (m memStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m[key] = b
	return nil
}
func (m memStore) CreateMultipart(context.Context, string) (string, error) { return "", nil }
func (m memStore) PresignPart(context.Context, string, string, int32, time.Duration) (string, error) {
	return "", nil
}
func (m memStore) CompleteMultipart(context.Context, string, string, []storage.Part) error {
	return nil
}
func (m memStore) AbortMultipart(context.Context, string, string) error { return nil }
func (m memStore) Stat(_ context.Context, key string) (int64, error) {
	b, ok := m[key]
	if !ok {
		return 0, errors.New("not found")
	}
	return int64(len(b)), nil
}
func (m memStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := m[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m memStore) Delete(_ context.Context, key string) error {
	delete(m, key)
	return nil
}
func (m memStore) PublicURL(key string) string { return key }

var _ storage.Storage = memStore{}
