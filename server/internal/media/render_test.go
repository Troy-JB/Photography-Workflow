package media

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func gradientPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	wm, err := NewWatermark("")
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	return &Service{wm: wm}
}

func TestRenderSizesAndLQIP(t *testing.T) {
	svc := newTestService(t)
	res, err := svc.Render(gradientPNG(t, 4000, 3000), "PWF Studio")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if res.ThumbWidth > ThumbEdge || res.ThumbHeight > ThumbEdge {
		t.Fatalf("thumb %dx%d exceeds %d edge", res.ThumbWidth, res.ThumbHeight, ThumbEdge)
	}
	if res.DisplayWidth != DisplayEdge {
		t.Fatalf("display width = %d, want %d", res.DisplayWidth, DisplayEdge)
	}
	if res.DisplayHeight >= res.DisplayWidth {
		t.Fatalf("landscape display should stay landscape, got %dx%d", res.DisplayWidth, res.DisplayHeight)
	}
	if res.SourceWidth != 4000 || res.SourceHeight != 3000 {
		t.Fatalf("source = %dx%d", res.SourceWidth, res.SourceHeight)
	}
	if !strings.HasPrefix(res.LQIP, "data:image/jpeg;base64,") {
		t.Fatalf("lqip data uri missing: %.40q", res.LQIP)
	}
	if len(res.Color) != 7 || res.Color[0] != '#' {
		t.Fatalf("color = %q, want #rrggbb", res.Color)
	}
	if len(res.Thumb) == 0 || len(res.Display) == 0 {
		t.Fatal("missing encoded output")
	}
}

func TestRenderDoesNotUpscale(t *testing.T) {
	svc := newTestService(t)
	res, err := svc.Render(gradientPNG(t, 320, 200), "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if res.DisplayWidth != 320 || res.DisplayHeight != 200 {
		t.Fatalf("display = %dx%d, want original 320x200", res.DisplayWidth, res.DisplayHeight)
	}
}

func TestRenderRejectsTinyInput(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.Render(gradientPNG(t, 1, 1), ""); err == nil {
		t.Fatal("expected an error for a 1x1 image")
	}
	if _, err := svc.Render([]byte("not an image"), ""); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestDerivableSourceDetection(t *testing.T) {
	if !derivableMime("image/jpeg") || !derivableMime("IMAGE/PNG") {
		t.Fatal("jpeg/png must be derivable")
	}
	if derivableMime("image/x-canon-cr3") || derivableMime("") {
		t.Fatal("raw must not be derivable on the server")
	}
	if !derivableExtension("a/b/IMG_0001.JPG") || !derivableExtension("x.png") {
		t.Fatal("jpg/png extensions must be derivable")
	}
	if derivableExtension("IMG_0001.CR3") {
		t.Fatal("raw extension must not be derivable")
	}
}

func TestDerivativeObjectKey(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	key := DerivativeObjectKey(uuid.Nil, uuid.Nil, hash, KindThumb)
	want := "t/00000000-0000-0000-0000-000000000000/p/00000000-0000-0000-0000-000000000000/ab/" + hash + "/thumb.jpg"
	if key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
}

func TestRevisionObjectKey(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	asset := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	key := RevisionObjectKey(tenant, asset, 3, KindDisplay)
	want := "t/11111111-1111-1111-1111-111111111111/revisions/22222222-2222-2222-2222-222222222222/v3/display.jpg"
	if key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
	thumb := RevisionObjectKey(tenant, asset, 1, KindThumb)
	if thumb == key || !strings.HasSuffix(thumb, "/v1/thumb.jpg") {
		t.Fatalf("thumb key = %q", thumb)
	}
}
