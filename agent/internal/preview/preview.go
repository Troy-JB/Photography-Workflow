// Package preview builds the small JPEGs the agent UI shows. RAW files use
// the camera's embedded JPEG; ordinary images are resized in pure Go; videos
// and HEIC go through ffmpeg. Nothing here is uploaded.
package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/disintegration/imaging"

	"photography-workflow/agent/internal/meta"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/tool"
)

const (
	// Edge is the longest side of a cached preview.
	Edge = 512
	// quality keeps a 512px preview around 30-60 KiB.
	quality = 82

	// GalleryEdge is the longest side the server renders galleries from. It is
	// the only way a RAW file reaches a customer gallery: the embedded preview
	// is uploaded and every smaller size is derived server-side.
	GalleryEdge    = 2560
	galleryQuality = 82
)

var (
	ErrNoHash            = errors.New("preview: file has no verified hash")
	ErrUnsupported       = errors.New("preview: unsupported media type")
	ErrFFmpegUnavailable = errors.New("preview: ffmpeg is not available")
	ErrEmptyFrame        = errors.New("preview: ffmpeg produced no frame")
)

// Generator writes previews into CacheDir keyed by the verified SHA-256.
type Generator struct {
	Reader   meta.Reader // required for RAW embedded previews
	FFmpeg   string
	CacheDir string
}

// Path returns the cache location for a content hash.
func (g *Generator) Path(sha string) string {
	return filepath.Join(g.CacheDir, sha, "512.jpg")
}

// GalleryPath returns the cache location of the uploadable gallery JPEG.
func (g *Generator) GalleryPath(sha string) string {
	return filepath.Join(g.CacheDir, sha, "gallery.jpg")
}

// GalleryPreview describes the uploadable preview. SHA256 hashes the JPEG bytes
// (not the original file) because the server stores it content addressed.
type GalleryPreview struct {
	Path   string
	Width  int
	Height int
	Bytes  int64
	SHA256 string
}

// Gallery renders (or reuses) the long-edge 2560 JPEG that feeds the customer
// gallery. RAW and HEIC go through the embedded preview / ffmpeg, exactly like
// the UI thumbnail, so the result is a plain JPEG in every case.
func (g *Generator) Gallery(ctx context.Context, f model.File, srcPath string) (*GalleryPreview, error) {
	sha := strings.ToLower(strings.TrimSpace(f.SHA256))
	if sha == "" {
		return nil, ErrNoHash
	}
	if f.Kind == model.KindSidecar || f.Role == model.RoleSidecar {
		return nil, ErrUnsupported
	}
	dst := g.GalleryPath(sha)
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return describeGallery(dst)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	img, err := g.sourceImage(ctx, f, srcPath, GalleryEdge)
	if err != nil {
		return nil, err
	}
	img = fitEdge(img, GalleryEdge)
	data, err := encodeQuality(img, galleryQuality)
	if err != nil {
		return nil, err
	}
	if !meta.IsJPEG(data) {
		return nil, fmt.Errorf("preview: gallery of %s: %w", f.RelPath, meta.ErrInvalidImage)
	}
	if err := writeAtomic(dst, data); err != nil {
		return nil, err
	}
	return describeGallery(dst)
}

func describeGallery(path string) (*GalleryPreview, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("preview: read gallery: %w", err)
	}
	if !meta.IsJPEG(data) {
		return nil, fmt.Errorf("preview: %s: %w", filepath.Base(path), meta.ErrInvalidImage)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("preview: decode gallery header: %w", err)
	}
	sum := sha256.Sum256(data)
	return &GalleryPreview{
		Path: path, Width: cfg.Width, Height: cfg.Height,
		Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// sourceImage resolves a media file to a decodable image, bounded by maxEdge
// where the source allows it (ffmpeg scales while decoding).
func (g *Generator) sourceImage(ctx context.Context, f model.File, srcPath string, maxEdge int) (image.Image, error) {
	switch {
	case IsRAW(f):
		if g.Reader == nil {
			return nil, errors.New("preview: RAW preview requires exiftool")
		}
		data, err := meta.EmbeddedPreview(ctx, g.Reader, srcPath, meta.PreviewTags)
		if err != nil {
			return nil, err
		}
		img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
		if err != nil {
			return nil, fmt.Errorf("preview: decode embedded JPEG: %w", err)
		}
		return img, nil
	case isVideo(f), isHEIC(f):
		data, err := g.ffmpegFrame(ctx, srcPath, !isHEIC(f), maxEdge)
		if err != nil {
			return nil, err
		}
		img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
		if err != nil {
			return nil, fmt.Errorf("preview: decode frame: %w", err)
		}
		return img, nil
	default:
		img, err := imaging.Open(srcPath, imaging.AutoOrientation(true))
		if err != nil {
			return nil, fmt.Errorf("preview: decode %s: %w", filepath.Base(srcPath), err)
		}
		return img, nil
	}
}

// Preview returns the cached preview path, generating it on first request.
func (g *Generator) Preview(ctx context.Context, f model.File, srcPath string) (string, error) {
	sha := strings.ToLower(strings.TrimSpace(f.SHA256))
	if sha == "" {
		return "", ErrNoHash
	}
	dst := g.Path(sha)
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return dst, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.Kind == model.KindSidecar || f.Role == model.RoleSidecar {
		return "", ErrUnsupported
	}

	var data []byte
	var err error
	switch {
	case IsRAW(f):
		data, err = g.rawPreview(ctx, srcPath)
	case isVideo(f):
		data, err = g.ffmpegFrame(ctx, srcPath, true, Edge)
	case isHEIC(f):
		data, err = g.ffmpegFrame(ctx, srcPath, false, Edge)
	default:
		data, err = g.imagePreview(srcPath)
	}
	if err != nil {
		return "", err
	}
	if !meta.IsJPEG(data) {
		return "", fmt.Errorf("preview: %s: %w", f.RelPath, meta.ErrInvalidImage)
	}
	if err := writeAtomic(dst, data); err != nil {
		return "", err
	}
	return dst, nil
}

func (g *Generator) rawPreview(ctx context.Context, srcPath string) ([]byte, error) {
	if g.Reader == nil {
		return nil, errors.New("preview: RAW preview requires exiftool")
	}
	data, err := meta.EmbeddedPreview(ctx, g.Reader, srcPath, meta.PreviewTags)
	if err != nil {
		return nil, err
	}
	img, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("preview: decode embedded JPEG: %w", err)
	}
	return encode(fit(img))
}

func (g *Generator) imagePreview(srcPath string) ([]byte, error) {
	img, err := imaging.Open(srcPath, imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("preview: decode %s: %w", filepath.Base(srcPath), err)
	}
	return encode(fit(img))
}

func fit(img image.Image) image.Image {
	return fitEdge(img, Edge)
}

func fitEdge(img image.Image, edge int) image.Image {
	b := img.Bounds()
	if b.Dx() <= edge && b.Dy() <= edge {
		return img
	}
	return imaging.Fit(img, edge, edge, imaging.Lanczos)
}

func (g *Generator) ffmpegFrame(ctx context.Context, srcPath string, seek bool, edge int) ([]byte, error) {
	if g.FFmpeg == "" {
		return nil, ErrFFmpegUnavailable
	}
	attempts := []bool{seek}
	if seek {
		attempts = append(attempts, false) // very short clips may not have 1s.
	}
	for _, at := range attempts {
		data, err := g.runFFmpeg(ctx, srcPath, at, edge)
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			if !meta.IsJPEG(data) {
				return nil, meta.ErrInvalidImage
			}
			return data, nil
		}
	}
	return nil, ErrEmptyFrame
}

func (g *Generator) runFFmpeg(ctx context.Context, srcPath string, seek bool, edge int) ([]byte, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if seek {
		args = append(args, "-ss", "1")
	}
	args = append(args,
		"-i", srcPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2:force_original_aspect_ratio=decrease", edge),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-",
	)
	cmd := exec.CommandContext(ctx, g.FFmpeg, args...)
	tool.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("preview: ffmpeg: %w (%s)", err,
			strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func encode(img image.Image) ([]byte, error) {
	return encodeQuality(img, quality)
}

func encodeQuality(img image.Image, q int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, fmt.Errorf("preview: encode: %w", err)
	}
	return buf.Bytes(), nil
}

func writeAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("preview: create cache: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".preview-*.tmp")
	if err != nil {
		return fmt.Errorf("preview: temp file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("preview: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("preview: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("preview: close: %w", err)
	}
	if err := os.Rename(name, dst); err != nil {
		// Another request may have produced the same preview first.
		if info, statErr := os.Stat(dst); statErr == nil && info.Size() > 0 {
			return nil
		}
		return fmt.Errorf("preview: install: %w", err)
	}
	return nil
}

func IsRAW(f model.File) bool {
	if f.Role == model.RoleRAW {
		return true
	}
	switch strings.ToLower(filepath.Ext(f.RelPath)) {
	case ".cr2", ".cr3", ".nef", ".arw", ".dng", ".raf", ".orf", ".rw2":
		return true
	}
	return false
}

func isVideo(f model.File) bool {
	if f.Kind == model.KindVideo || f.Role == model.RoleVideo {
		return true
	}
	switch strings.ToLower(filepath.Ext(f.RelPath)) {
	case ".mp4", ".mov", ".mts", ".m2ts", ".avi", ".mkv", ".webm":
		return true
	}
	return false
}

func isHEIC(f model.File) bool {
	switch strings.ToLower(filepath.Ext(f.RelPath)) {
	case ".heic", ".heif":
		return true
	}
	return strings.EqualFold(f.Mime, "image/heic") || strings.EqualFold(f.Mime, "image/heif")
}
