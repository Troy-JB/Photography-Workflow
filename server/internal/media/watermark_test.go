package media

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func blankCanvas(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 90, G: 90, B: 90, A: 255})
		}
	}
	return img
}

func changedPixels(img *image.RGBA) int {
	n := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if img.RGBAAt(x, y) != (color.RGBA{R: 90, G: 90, B: 90, A: 255}) {
				n++
			}
		}
	}
	return n
}

func TestWatermarkDrawsBottomRight(t *testing.T) {
	wm, err := NewWatermark("")
	if err != nil {
		t.Fatalf("fallback font: %v", err)
	}
	img := blankCanvas(1600, 1000)
	if err := wm.Draw(img, "PWF Studio"); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if changedPixels(img) == 0 {
		t.Fatal("watermark did not change any pixel")
	}
	// The top-left quadrant must stay untouched: the mark lives bottom-right.
	topLeft := img.SubImage(image.Rect(0, 0, 800, 500)).(*image.RGBA)
	if changedPixels(topLeft) != 0 {
		t.Fatal("watermark leaked into the top-left quadrant")
	}
}

func TestWatermarkEmptyTextIsNoop(t *testing.T) {
	wm, err := NewWatermark("")
	if err != nil {
		t.Fatalf("fallback font: %v", err)
	}
	img := blankCanvas(400, 300)
	if err := wm.Draw(img, "   "); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if changedPixels(img) != 0 {
		t.Fatal("blank text must not draw")
	}
}

func TestWatermarkLongTextShrinks(t *testing.T) {
	wm, err := NewWatermark("")
	if err != nil {
		t.Fatalf("fallback font: %v", err)
	}
	img := blankCanvas(800, 600)
	long := strings.Repeat("PWF STUDIO ", 12)
	if err := wm.Draw(img, long); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if changedPixels(img) == 0 {
		t.Fatal("long watermark did not draw")
	}
	// The rightmost pixel column must stay clean: the text fits inside the
	// image instead of running off the edge.
	for y := 0; y < 600; y++ {
		if img.RGBAAt(799, y) != (color.RGBA{R: 90, G: 90, B: 90, A: 255}) {
			t.Fatalf("watermark touched the right edge at y=%d", y)
		}
	}
}

func TestNewWatermarkRejectsMissingFont(t *testing.T) {
	_, err := NewWatermark(filepath.Join(t.TempDir(), "absent.ttf"))
	if err == nil || !strings.Contains(err.Error(), "watermark font") {
		t.Fatalf("error = %v, want a font read failure", err)
	}
}

func TestNewWatermarkRejectsGarbageFont(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fake.ttf")
	if err := os.WriteFile(p, []byte("definitely not a font"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewWatermark(p); err == nil {
		t.Fatal("expected a parse failure for garbage font data")
	}
}
