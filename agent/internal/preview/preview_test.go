package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/disintegration/imaging"

	"photography-workflow/agent/internal/meta"
	"photography-workflow/agent/internal/model"
)

func shaHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := imaging.New(w, h, color.NRGBA{R: 40, G: 90, B: 160, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	return cfg.Width, cfg.Height
}

type fakeMeta struct {
	data []byte
	err  error
}

func (f *fakeMeta) JSON(context.Context, string) ([]byte, error) { return nil, errors.New("unused") }
func (f *fakeMeta) Binary(context.Context, string, []string) ([]byte, error) {
	return f.data, f.err
}
func (f *fakeMeta) Close() error { return nil }

func TestJPEGPreviewResizesAndReusesCache(t *testing.T) {
	src := writeTemp(t, "IMG.JPG", jpegBytes(t, 1200, 800))
	g := &Generator{CacheDir: t.TempDir()}
	f := model.File{RelPath: "IMG.JPG", Role: model.RoleJPEG, Kind: model.KindPhoto,
		SHA256: strings.Repeat("a", 64)}

	path, err := g.Preview(context.Background(), f, src)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	w, h := decodeSize(t, path)
	if w != 512 || h != 341 {
		t.Fatalf("preview size = %dx%d, want 512x341", w, h)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	again, err := g.Preview(context.Background(), f, src)
	if err != nil || again != path {
		t.Fatalf("cached preview: %v %q", err, again)
	}
	if st, err := os.Stat(again); err != nil || !st.ModTime().Equal(info.ModTime()) {
		t.Fatalf("cache was regenerated: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(g.CacheDir, f.SHA256) {
		t.Fatalf("cache layout = %s", path)
	}
}

func TestPNGPreviewFitsPortrait(t *testing.T) {
	img := imaging.New(600, 1200, color.NRGBA{G: 200, A: 255})
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
		t.Fatal(err)
	}
	src := writeTemp(t, "IMG.PNG", buf.Bytes())
	g := &Generator{CacheDir: t.TempDir()}
	f := model.File{RelPath: "IMG.PNG", Role: model.RoleImage, Kind: model.KindPhoto,
		SHA256: strings.Repeat("b", 64)}
	path, err := g.Preview(context.Background(), f, src)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	w, h := decodeSize(t, path)
	if w != 256 || h != 512 {
		t.Fatalf("preview size = %dx%d, want 256x512", w, h)
	}
}

func TestSmallImageIsNotUpscaled(t *testing.T) {
	src := writeTemp(t, "SMALL.JPG", jpegBytes(t, 100, 80))
	g := &Generator{CacheDir: t.TempDir()}
	f := model.File{RelPath: "SMALL.JPG", Role: model.RoleJPEG, Kind: model.KindPhoto,
		SHA256: strings.Repeat("c", 64)}
	path, err := g.Preview(context.Background(), f, src)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	w, h := decodeSize(t, path)
	if w != 100 || h != 80 {
		t.Fatalf("preview size = %dx%d, want 100x80", w, h)
	}
}

func TestRAWUsesEmbeddedJPEG(t *testing.T) {
	embedded := jpegBytes(t, 1600, 1067)
	g := &Generator{CacheDir: t.TempDir(), Reader: &fakeMeta{data: embedded}}
	f := model.File{RelPath: "IMG_0001.CR3", Role: model.RoleRAW, Kind: model.KindPhoto,
		SHA256: strings.Repeat("d", 64)}
	path, err := g.Preview(context.Background(), f, "C:/cards/IMG_0001.CR3")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	w, h := decodeSize(t, path)
	if w != 512 || h != 341 {
		t.Fatalf("preview size = %dx%d, want 512x341", w, h)
	}

	g.Reader = &fakeMeta{data: []byte{0x00, 0x01}}
	if _, err := g.Preview(context.Background(), model.File{
		RelPath: "IMG_0002.CR3", Role: model.RoleRAW, SHA256: strings.Repeat("e", 64),
	}, "x"); !errors.Is(err, meta.ErrInvalidImage) {
		t.Fatalf("invalid embedded preview err = %v", err)
	}
}

func buildFFmpeg(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	name := "fakeffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakeffmpeg")
	cmd.Dir = dir
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake ffmpeg: %v\n%s", err, combined)
	}
	return out
}

func TestVideoPosterUsesFFmpeg(t *testing.T) {
	frame := jpegBytes(t, 1920, 1080)
	framePath := writeTemp(t, "frame.jpg", frame)
	t.Setenv("FAKE_FFMPEG_FRAME", framePath)
	src := writeTemp(t, "CLIP.MP4", []byte("not a real video"))
	g := &Generator{CacheDir: t.TempDir(), FFmpeg: buildFFmpeg(t)}
	f := model.File{RelPath: "CLIP.MP4", Role: model.RoleVideo, Kind: model.KindVideo,
		SHA256: strings.Repeat("f", 64)}
	path, err := g.Preview(context.Background(), f, src)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	w, h := decodeSize(t, path)
	if w != 1920 || h != 1080 {
		t.Fatalf("poster size = %dx%d", w, h)
	}
}

func TestFFmpegFailureAndMissingTool(t *testing.T) {
	src := writeTemp(t, "CLIP.MOV", []byte("x"))
	g := &Generator{CacheDir: t.TempDir()}
	f := model.File{RelPath: "CLIP.MOV", Role: model.RoleVideo, Kind: model.KindVideo,
		SHA256: strings.Repeat("1", 64)}
	if _, err := g.Preview(context.Background(), f, src); !errors.Is(err, ErrFFmpegUnavailable) {
		t.Fatalf("missing ffmpeg err = %v", err)
	}

	t.Setenv("FAKE_FFMPEG_FAIL", "1")
	g.FFmpeg = buildFFmpeg(t)
	if _, err := g.Preview(context.Background(), f, src); err == nil {
		t.Fatal("ffmpeg failure must surface")
	}
}

func TestUnsupportedAndMissingHash(t *testing.T) {
	g := &Generator{CacheDir: t.TempDir()}
	if _, err := g.Preview(context.Background(), model.File{RelPath: "notes.txt"}, "x"); !errors.Is(err, ErrNoHash) {
		t.Fatalf("missing hash err = %v", err)
	}
	sidecar := model.File{RelPath: "IMG.XMP", Role: model.RoleSidecar, Kind: model.KindSidecar,
		SHA256: strings.Repeat("2", 64)}
	if _, err := g.Preview(context.Background(), sidecar, "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("sidecar err = %v", err)
	}
}

func TestGalleryResizesTo2560AndHashesBytes(t *testing.T) {
	src := writeTemp(t, "IMG.JPG", jpegBytes(t, 6000, 4000))
	g := &Generator{CacheDir: t.TempDir()}
	f := model.File{RelPath: "IMG.JPG", Role: model.RoleJPEG, Kind: model.KindPhoto,
		SHA256: strings.Repeat("3", 64)}

	pre, err := g.Gallery(context.Background(), f, src)
	if err != nil {
		t.Fatalf("gallery: %v", err)
	}
	if pre.Width != GalleryEdge || pre.Height < 1700 || pre.Height > 1710 {
		t.Fatalf("gallery size = %dx%d, want 2560x~1707", pre.Width, pre.Height)
	}
	data, err := os.ReadFile(pre.Path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != pre.Bytes || pre.SHA256 != shaHex(data) {
		t.Fatal("gallery hash/size do not describe the cached file")
	}
	if filepath.Base(pre.Path) != "gallery.jpg" {
		t.Fatalf("cache name = %s", pre.Path)
	}

	info, err := os.Stat(pre.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	again, err := g.Gallery(context.Background(), f, src)
	if err != nil {
		t.Fatalf("cached gallery: %v", err)
	}
	if st, err := os.Stat(again.Path); err != nil || !st.ModTime().Equal(info.ModTime()) {
		t.Fatalf("gallery cache regenerated: %v", err)
	}
}

func TestGalleryFromRAWAndRejectsSidecar(t *testing.T) {
	embedded := jpegBytes(t, 4000, 3000)
	g := &Generator{CacheDir: t.TempDir(), Reader: &fakeMeta{data: embedded}}
	f := model.File{RelPath: "IMG_0001.CR3", Role: model.RoleRAW, Kind: model.KindPhoto,
		SHA256: strings.Repeat("4", 64)}
	pre, err := g.Gallery(context.Background(), f, "C:/cards/IMG_0001.CR3")
	if err != nil {
		t.Fatalf("raw gallery: %v", err)
	}
	if pre.Width != 2560 {
		t.Fatalf("raw gallery width = %d", pre.Width)
	}

	sidecar := model.File{RelPath: "IMG.XMP", Role: model.RoleSidecar, Kind: model.KindSidecar,
		SHA256: strings.Repeat("5", 64)}
	if _, err := g.Gallery(context.Background(), sidecar, "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("sidecar err = %v", err)
	}
}
