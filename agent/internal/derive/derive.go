// Package derive fills in EXIF metadata after copies verify and serves
// on-demand previews. It is best-effort: a missing or broken helper must never
// fail an import that already has two verified local copies.
package derive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"photography-workflow/agent/internal/meta"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/preview"
	"photography-workflow/agent/internal/store"
)

// ErrExifUnavailable means no exiftool path is configured or it cannot start.
var ErrExifUnavailable = errors.New("derive: exiftool is not available")

// Derive owns the shared ExifTool process and the preview cache.
type Derive struct {
	Store    *store.Store
	CacheDir string
	OnEvent  func(model.Event)

	mu       sync.Mutex
	exifPath string
	ffmpeg   string
	reader   *readerRef
}

type readerRef struct {
	r       meta.Reader
	active  int
	retired bool
}

// New creates a deriver. Paths may be empty; the tool is only started when a
// file actually needs it.
func New(st *store.Store, cacheDir, exifPath, ffmpeg string, onEvent func(model.Event)) *Derive {
	return &Derive{
		Store: st, CacheDir: cacheDir,
		exifPath: strings.TrimSpace(exifPath), ffmpeg: strings.TrimSpace(ffmpeg),
		OnEvent: onEvent,
	}
}

// SetTools points the deriver at new helper binaries. A running ExifTool is
// retired after the calls that already use it finish.
func (d *Derive) SetTools(exifPath, ffmpeg string) {
	exifPath = strings.TrimSpace(exifPath)
	ffmpeg = strings.TrimSpace(ffmpeg)

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.exifPath == exifPath && d.ffmpeg == ffmpeg {
		return
	}
	d.exifPath = exifPath
	d.ffmpeg = ffmpeg
	if d.reader != nil {
		d.reader.retired = true
		if d.reader.active == 0 {
			_ = d.reader.r.Close()
			d.reader = nil
		}
	}
}

// Close stops the helper process.
func (d *Derive) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reader == nil {
		return nil
	}
	d.reader.retired = true
	if d.reader.active == 0 {
		err := d.reader.r.Close()
		d.reader = nil
		return err
	}
	return nil
}

func (d *Derive) acquireReader() (*readerRef, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reader != nil && !d.reader.retired {
		d.reader.active++
		return d.reader, nil
	}
	if d.exifPath == "" {
		return nil, ErrExifUnavailable
	}
	r, err := meta.Open(d.exifPath)
	if err != nil {
		return nil, err
	}
	ref := &readerRef{r: r, active: 1}
	d.reader = ref
	return ref, nil
}

func (d *Derive) releaseReader(ref *readerRef) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ref.active--
	if ref.active == 0 && ref.retired {
		_ = ref.r.Close()
		if d.reader == ref {
			d.reader = nil
		}
	}
}

// Enrich extracts metadata for every media file that does not have it yet.
// It reads from the verified A copy (falling back to B), so metadata is never
// taken from a file that failed verification.
func (d *Derive) Enrich(ctx context.Context, im *model.Import) error {
	files, err := d.Store.Files(ctx, im.ID)
	if err != nil {
		return err
	}
	pending := 0
	for _, f := range files {
		if needsMetadata(f) {
			pending++
		}
	}
	if pending == 0 {
		return nil
	}

	ref, err := d.acquireReader()
	if err != nil {
		d.emit(ctx, model.Event{
			Level: "warning", Code: "metadata_unavailable", ImportID: im.ID,
			Message: "未找到可用的 exiftool，EXIF 元数据将保持为空；安装包内的 tools\\exiftool.exe 或设置中的路径可修复。",
		})
		return nil
	}
	defer d.releaseReader(ref)

	failed := 0
	firstFailure := ""
	for _, f := range files {
		if !needsMetadata(f) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := d.verifiedPath(ctx, f.ID)
		if err != nil {
			failed++
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("%s：%v", f.RelPath, err)
			}
			continue
		}
		md, err := meta.Extract(ctx, ref.r, path)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failed++
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("%s：%v", f.RelPath, err)
			}
			continue
		}
		if err := d.Store.SetFileMetadata(ctx, f.ID, md); err != nil {
			return err
		}
	}
	if failed > 0 {
		d.emit(ctx, model.Event{
			Level: "warning", Code: "metadata_partial", ImportID: im.ID,
			Message: fmt.Sprintf("%d 个文件的 EXIF 读取失败（首个：%s）。文件与校验不受影响。",
				failed, firstFailure),
		})
	}
	return nil
}

// Preview returns the cached 512px JPEG for a file, generating it from the
// verified copy when needed.
func (d *Derive) Preview(ctx context.Context, f model.File) (string, error) {
	if f.Kind == model.KindSidecar || f.Kind == model.KindOther {
		return "", preview.ErrUnsupported
	}
	src, err := d.verifiedPath(ctx, f.ID)
	if err != nil {
		return "", err
	}
	gen := &preview.Generator{FFmpeg: d.ffmpeg, CacheDir: d.CacheDir}
	if preview.IsRAW(f) {
		ref, err := d.acquireReader()
		if err != nil {
			return "", d.previewFailure(ctx, f, err)
		}
		defer d.releaseReader(ref)
		gen.Reader = ref.r
	}
	path, err := gen.Preview(ctx, f, src)
	if err != nil {
		return "", d.previewFailure(ctx, f, err)
	}
	return path, nil
}

// Gallery renders the long-edge 2560 JPEG the server turns into a customer
// gallery. It uses the same helper handling as Preview and leaves event
// reporting to the caller: an upload failure is not always an operator problem.
func (d *Derive) Gallery(ctx context.Context, f model.File) (*preview.GalleryPreview, error) {
	if f.Kind == model.KindSidecar || f.Kind == model.KindOther {
		return nil, preview.ErrUnsupported
	}
	src, err := d.verifiedPath(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	ffmpeg := d.ffmpeg
	d.mu.Unlock()
	gen := &preview.Generator{FFmpeg: ffmpeg, CacheDir: d.CacheDir}
	if preview.IsRAW(f) {
		ref, err := d.acquireReader()
		if err != nil {
			return nil, toolReason(err)
		}
		defer d.releaseReader(ref)
		gen.Reader = ref.r
	}
	out, err := gen.Gallery(ctx, f, src)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, toolReason(err)
	}
	return out, nil
}

// toolReason turns a helper failure into an actionable message.
func toolReason(err error) error {
	switch {
	case errors.Is(err, ErrExifUnavailable):
		return errors.New("RAW 预览需要 exiftool：请在设置中填写路径，或把 exiftool.exe 放到安装目录的 tools 文件夹")
	case errors.Is(err, preview.ErrFFmpegUnavailable):
		return errors.New("视频/HEIC 预览需要 ffmpeg：请在设置中填写路径，或把 ffmpeg.exe 放到安装目录的 tools 文件夹")
	}
	return err
}

// previewFailure records the derivative event and returns a message the
// operator can act on instead of a raw helper error.
func (d *Derive) previewFailure(ctx context.Context, f model.File, err error) error {
	err = toolReason(err)
	reason := err.Error()
	d.emit(ctx, model.Event{
		Level: "warning", Code: "derivative_failed", ImportID: f.ImportID, FileID: f.ID,
		Message: fmt.Sprintf("%s：无法生成预览（%s）", f.RelPath, reason),
	})
	return errors.New(reason)
}

func needsMetadata(f model.File) bool {
	if f.Kind != model.KindPhoto && f.Kind != model.KindVideo {
		return false
	}
	return f.ExifJSON == ""
}

// verifiedPath prefers the primary copy; both paths were hash-checked before
// the file reached a local state.
func (d *Derive) verifiedPath(ctx context.Context, fileID string) (string, error) {
	copies, err := d.Store.Copies(ctx, fileID)
	if err != nil {
		return "", err
	}
	fallback := ""
	for _, c := range copies {
		if c.State != model.CopyVerified || c.FinalPath == "" {
			continue
		}
		if c.Dest == model.DestA {
			return c.FinalPath, nil
		}
		if fallback == "" {
			fallback = c.FinalPath
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errors.New("没有已校验的本地副本")
}

func (d *Derive) emit(ctx context.Context, e model.Event) {
	_, _ = d.Store.AddEvent(ctx, e)
	if d.OnEvent != nil {
		d.OnEvent(e)
	}
}
