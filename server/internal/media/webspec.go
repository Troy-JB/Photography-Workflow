package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"path"
	"strings"

	"github.com/disintegration/imaging"
)

// ErrWebSpecDecode marks a source the server cannot decode for the web
// package. The delivery build records a warning and keeps going unless the
// deployment requires a web spec for every asset.
var ErrWebSpecDecode = errors.New("image cannot be decoded for the web package")

// WebSpec is the no-watermark JPEG placed in the customer web package.
type WebSpec struct {
	Bytes  []byte
	Width  int
	Height int
	SHA256 string
}

// RenderWebSpec fits src into a square of edge pixels, flattens transparency
// onto white and encodes a JPEG at the shared preview quality. It never draws
// the gallery watermark: this is the file the customer paid for.
func RenderWebSpec(src []byte, edge int) (*WebSpec, error) {
	if len(src) == 0 {
		return nil, fmt.Errorf("%w: empty source", ErrWebSpecDecode)
	}
	if len(src) > maxSourceBytes {
		return nil, fmt.Errorf("%w: source exceeds %d bytes", ErrWebSpecDecode, maxSourceBytes)
	}
	if edge < 512 {
		edge = DisplayEdge
	}
	img, err := imaging.Decode(bytes.NewReader(src), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWebSpecDecode, err)
	}
	bounds := img.Bounds()
	if bounds.Dx() < 2 || bounds.Dy() < 2 {
		return nil, fmt.Errorf("%w: image is too small (%dx%d)", ErrWebSpecDecode, bounds.Dx(), bounds.Dy())
	}
	fit := imaging.Fit(flattenWhite(img), edge, edge, imaging.Lanczos)
	out, err := encodeJPEG(fit, jpegQuality)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWebSpecDecode, err)
	}
	sum := sha256.Sum256(out)
	return &WebSpec{
		Bytes:  out,
		Width:  fit.Bounds().Dx(),
		Height: fit.Bounds().Dy(),
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// flattenWhite paints transparent pixels onto white; JPEG has no alpha and
// would otherwise render them black.
func flattenWhite(img image.Image) image.Image {
	bounds := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Over)
	return dst
}

// WebSpecUnsupported reports formats this build cannot decode on the server
// (HEIC/HEIF, TIFF and camera RAW). They still travel byte-for-byte in the
// original package; only the web derivative is skipped with a warning.
func WebSpecUnsupported(mime, filename string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/heic", "image/heif", "image/tiff",
		"image/x-adobe-dng", "image/x-canon-cr2", "image/x-canon-cr3",
		"image/x-nikon-nef", "image/x-sony-arw", "image/x-fuji-raf",
		"image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-pentax-pef",
		"image/x-samsung-srw", "image/x-sigma-x3f":
		return true
	}
	switch strings.ToLower(path.Ext(strings.ReplaceAll(filename, "\\", "/"))) {
	case ".heic", ".heif", ".tif", ".tiff",
		".dng", ".cr2", ".cr3", ".nef", ".arw", ".raf",
		".orf", ".rw2", ".pef", ".srw", ".x3f", ".raw":
		return true
	}
	return false
}
