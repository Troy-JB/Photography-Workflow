package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func TestRenderWebSpecFitsLongEdge(t *testing.T) {
	spec, err := RenderWebSpec(gradientPNG(t, 4000, 3000), 2048)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if spec.Width != 2048 || spec.Height != 1536 {
		t.Fatalf("size = %dx%d, want 2048x1536", spec.Width, spec.Height)
	}
	if len(spec.Bytes) < 2 || spec.Bytes[0] != 0xFF || spec.Bytes[1] != 0xD8 {
		t.Fatal("web spec is not a JPEG")
	}
	sum := sha256.Sum256(spec.Bytes)
	if spec.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha = %q, want %q", spec.SHA256, hex.EncodeToString(sum[:]))
	}
}

func TestRenderWebSpecDoesNotUpscale(t *testing.T) {
	spec, err := RenderWebSpec(gradientPNG(t, 320, 200), 2048)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if spec.Width != 320 || spec.Height != 200 {
		t.Fatalf("size = %dx%d, want 320x200", spec.Width, spec.Height)
	}
}

func TestRenderWebSpecAppliesEXIFOrientation(t *testing.T) {
	src := jpegWithOrientation(t, solidImage(400, 200, color.RGBA{200, 40, 40, 255}), 6)
	spec, err := RenderWebSpec(src, 512)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if spec.Width != 200 || spec.Height != 400 {
		t.Fatalf("size = %dx%d, want the orientation-rotated 200x400", spec.Width, spec.Height)
	}
}

func TestRenderWebSpecFlattensTransparencyToWhite(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	spec, err := RenderWebSpec(buf.Bytes(), 512)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	out, err := jpeg.Decode(bytes.NewReader(spec.Bytes))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	r, g, b, _ := out.At(32, 32).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("transparent pixel became %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}
}

func TestRenderWebSpecRejectsBadSources(t *testing.T) {
	if _, err := RenderWebSpec(nil, 2048); !errors.Is(err, ErrWebSpecDecode) {
		t.Fatalf("empty source err = %v", err)
	}
	if _, err := RenderWebSpec([]byte("not an image"), 2048); !errors.Is(err, ErrWebSpecDecode) {
		t.Fatalf("garbage err = %v", err)
	}
	var tiny bytes.Buffer
	if err := png.Encode(&tiny, solidImage(1, 1, color.White)); err != nil {
		t.Fatalf("encode tiny: %v", err)
	}
	if _, err := RenderWebSpec(tiny.Bytes(), 2048); !errors.Is(err, ErrWebSpecDecode) {
		t.Fatalf("1x1 err = %v", err)
	}
}

func TestRenderWebSpecRejectsOversizeSource(t *testing.T) {
	_, err := RenderWebSpec(make([]byte, maxSourceBytes+1), 2048)
	if !errors.Is(err, ErrWebSpecDecode) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize err = %v", err)
	}
}

func TestWebSpecUnsupported(t *testing.T) {
	cases := []struct {
		mime string
		name string
		want bool
	}{
		{"image/heic", "IMG_0001.HEIC", true},
		{"image/x-canon-cr3", "IMG_0001.CR3", true},
		{"", "IMG_0001.NEF", true},
		{"image/jpeg", "IMG_0001.jpg", false},
		{"image/png", "scan.png", false},
		{"", "archive.tiff", true},
		{"image/webp", "retouch.webp", false},
	}
	for _, tc := range cases {
		if got := WebSpecUnsupported(tc.mime, tc.name); got != tc.want {
			t.Errorf("WebSpecUnsupported(%q, %q) = %v, want %v", tc.mime, tc.name, got, tc.want)
		}
	}
}

func solidImage(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

// jpegWithOrientation inserts a minimal EXIF APP1 segment so the decoder has
// to honour the orientation tag.
func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	data := buf.Bytes()
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		t.Fatal("encoder did not produce a JPEG")
	}
	tiff := []byte{
		'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00,
		0x01, 0x00,
		0x12, 0x01,
		0x03, 0x00,
		0x01, 0x00, 0x00, 0x00,
		byte(orientation), byte(orientation >> 8), 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	length := len(payload) + 2
	seg := []byte{0xFF, 0xE1, byte(length >> 8), byte(length)}
	seg = append(seg, payload...)

	out := make([]byte, 0, len(data)+len(seg))
	out = append(out, data[:2]...)
	out = append(out, seg...)
	out = append(out, data[2:]...)
	return out
}
