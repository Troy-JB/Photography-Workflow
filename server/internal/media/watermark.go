package media

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Watermark draws the studio name over display previews. The font is parsed
// once at startup; an unreadable font is a boot error instead of a silent
// fallback, because a gallery without a watermark is a product defect.
type Watermark struct {
	font *opentype.Font
}

// NewWatermark loads fontPath (TTF/OTF/TTC). An empty path uses the embedded
// Go Regular fallback, which only covers Latin glyphs.
func NewWatermark(fontPath string) (*Watermark, error) {
	data := goregular.TTF
	if strings.TrimSpace(fontPath) != "" {
		raw, err := os.ReadFile(fontPath)
		if err != nil {
			return nil, fmt.Errorf("read watermark font: %w", err)
		}
		data = raw
	}
	f, err := parseFont(data)
	if err != nil {
		return nil, fmt.Errorf("parse watermark font: %w", err)
	}
	return &Watermark{font: f}, nil
}

func parseFont(data []byte) (*opentype.Font, error) {
	if f, err := opentype.Parse(data); err == nil {
		return f, nil
	}
	coll, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	if coll.NumFonts() == 0 {
		return nil, fmt.Errorf("font collection is empty")
	}
	return coll.Font(0)
}

// Draw renders text in the bottom-right corner with a dark outline so it stays
// legible on both bright and dark photographs. Long text shrinks to fit 78% of
// the image width instead of running off the edge.
func (w *Watermark) Draw(dst draw.Image, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	bounds := dst.Bounds()
	short := math.Min(float64(bounds.Dx()), float64(bounds.Dy()))
	size := math.Max(14, short*0.035)
	maxWidth := int(float64(bounds.Dx()) * 0.78)

	face, err := w.face(size)
	if err != nil {
		return err
	}
	for font.MeasureString(face, text).Ceil() > maxWidth && size > 12 {
		face.Close()
		size--
		if face, err = w.face(size); err != nil {
			return err
		}
	}
	defer face.Close()
	// Even at the minimum size a long studio name may not fit; truncating keeps
	// the mark inside the frame instead of bleeding off the edge.
	text = fitText(face, text, maxWidth)

	width := font.MeasureString(face, text).Ceil()
	metrics := face.Metrics()
	height := metrics.Ascent.Ceil() + metrics.Descent.Ceil()
	margin := int(math.Max(8, short*0.025))
	x := bounds.Max.X - margin - width
	y := bounds.Max.Y - margin - height + metrics.Ascent.Ceil()
	if x < bounds.Min.X {
		x = bounds.Min.X + margin
	}
	if y < bounds.Min.Y+height {
		y = bounds.Min.Y + height
	}

	outline := image.NewUniform(color.NRGBA{R: 0, G: 0, B: 0, A: 170})
	for _, off := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}, {-1, -1}, {1, 1}, {-1, 1}, {1, -1}} {
		drawer := font.Drawer{Dst: dst, Src: outline, Face: face, Dot: fixed.P(x+off[0], y+off[1])}
		drawer.DrawString(text)
	}
	fill := image.NewUniform(color.NRGBA{R: 255, G: 255, B: 255, A: 150})
	drawer := font.Drawer{Dst: dst, Src: fill, Face: face, Dot: fixed.P(x, y)}
	drawer.DrawString(text)
	return nil
}

func (w *Watermark) face(size float64) (font.Face, error) {
	return opentype.NewFace(w.font, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
}

// fitText trims text rune by rune until it fits maxWidth, ending in an ellipsis.
func fitText(face font.Face, text string, maxWidth int) string {
	if font.MeasureString(face, text).Ceil() <= maxWidth {
		return text
	}
	runes := []rune(text)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		candidate := string(runes) + "…"
		if font.MeasureString(face, candidate).Ceil() <= maxWidth {
			return candidate
		}
	}
	return "…"
}
