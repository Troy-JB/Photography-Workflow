package media

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"path"
	"strings"

	"github.com/disintegration/imaging"
	"github.com/google/uuid"
	// Register the WebP decoder for retouch revisions; originals stay JPEG/PNG.
	_ "golang.org/x/image/webp"
)

const (
	// ThumbEdge feeds the virtual-scroll grid; DisplayEdge feeds the lightbox.
	ThumbEdge   = 640
	DisplayEdge = 2048
	lqipEdge    = 24

	jpegQuality = 82
	lqipQuality = 45
)

// Derivative kinds stored in asset_derivatives.kind.
const (
	KindGallery = "gallery"
	KindThumb   = "thumb"
	KindDisplay = "display"
)

// Rendered is the in-memory result of one source image.
type Rendered struct {
	Thumb       []byte
	Display     []byte
	ThumbWidth  int
	ThumbHeight int
	// DisplayWidth/DisplayHeight describe the watermarked preview.
	DisplayWidth  int
	DisplayHeight int
	SourceWidth   int
	SourceHeight  int
	// LQIP is a data URI of a 24px JPEG; Color is the average colour.
	LQIP  string
	Color string
}

// Render decodes src (JPEG/PNG), produces the grid thumbnail, the watermarked
// display preview and the low-quality placeholder.
func (s *Service) Render(src []byte, watermark string) (*Rendered, error) {
	img, err := imaging.Decode(bytes.NewReader(src), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() < 2 || bounds.Dy() < 2 {
		return nil, fmt.Errorf("image is too small (%dx%d)", bounds.Dx(), bounds.Dy())
	}

	thumb := imaging.Fit(img, ThumbEdge, ThumbEdge, imaging.Lanczos)
	display := imaging.Fit(img, DisplayEdge, DisplayEdge, imaging.Lanczos)
	if err := s.wm.Draw(display, watermark); err != nil {
		return nil, fmt.Errorf("draw watermark: %w", err)
	}

	thumbBytes, err := encodeJPEG(thumb, jpegQuality)
	if err != nil {
		return nil, err
	}
	displayBytes, err := encodeJPEG(display, jpegQuality)
	if err != nil {
		return nil, err
	}
	lqip := imaging.Fit(img, lqipEdge, lqipEdge, imaging.Lanczos)
	lqipBytes, err := encodeJPEG(lqip, lqipQuality)
	if err != nil {
		return nil, err
	}
	return &Rendered{
		Thumb:         thumbBytes,
		Display:       displayBytes,
		ThumbWidth:    thumb.Bounds().Dx(),
		ThumbHeight:   thumb.Bounds().Dy(),
		DisplayWidth:  display.Bounds().Dx(),
		DisplayHeight: display.Bounds().Dy(),
		SourceWidth:   bounds.Dx(),
		SourceHeight:  bounds.Dy(),
		LQIP:          "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(lqipBytes),
		Color:         averageColor(img),
	}, nil
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(quality)); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

func averageColor(img image.Image) string {
	one := imaging.Resize(img, 1, 1, imaging.Box)
	r, g, b, _ := one.At(0, 0).RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}

// ObjectKey is the content-addressed layout shared with ingest. name is a
// fixed identifier (original/thumb/display/gallery), never user input.
func ObjectKey(tenantID, projectID uuid.UUID, hash, name string) string {
	return "t/" + tenantID.String() + "/p/" + projectID.String() + "/" +
		hash[:2] + "/" + hash + "/" + name + ".jpg"
}

// DerivativeObjectKey resolves the storage key for a known asset hash.
func DerivativeObjectKey(tenantID, projectID uuid.UUID, hash, kind string) string {
	return ObjectKey(tenantID, projectID, hash, kind)
}

func derivableMime(mime string) bool {
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/png":
		return true
	}
	// Fall back to the extension for older assets registered without a mime.
	return false
}

func derivableExtension(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png":
		return true
	}
	return false
}
