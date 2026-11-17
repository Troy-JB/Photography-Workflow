package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/storage"
)

// FolderWatch trades retouch work through directories on a local or mounted
// filesystem:
//
//	<root>/outgoing/<taskID>/manifest.json
//	<root>/outgoing/<taskID>/source/<assetID>.<ext>
//	<root>/incoming/<taskID>/<assetID>.jpg
//
// Export is idempotent: files are written through a temp name and renamed, and
// the manifest is written last so a consumer never reads a half-written job.
type FolderWatch struct {
	root  string
	store storage.Storage
}

func NewFolderWatch(root string, store storage.Storage) *FolderWatch {
	return &FolderWatch{root: filepath.Clean(root), store: store}
}

func (f *FolderWatch) Name() string { return ProviderFolderWatch }

// Root is exposed for the admin integration view.
func (f *FolderWatch) Root() string { return f.root }

const (
	folderOutgoing = "outgoing"
	folderIncoming = "incoming"
	folderSource   = "source"
	folderDone     = "done"
	folderFailed   = "failed"
	manifestName   = "manifest.json"
	manifestFormat = "pwf.retouch.v1"
)

type folderManifest struct {
	Format    string               `json:"format"`
	TaskID    uuid.UUID            `json:"task_id"`
	RoundID   uuid.UUID            `json:"round_id"`
	RoundNo   int                  `json:"round_no"`
	CreatedAt time.Time            `json:"created_at"`
	Note      string               `json:"note"`
	Items     []folderManifestItem `json:"items"`
}

type folderManifestItem struct {
	AssetID    uuid.UUID `json:"asset_id"`
	Filename   string    `json:"filename"`
	Position   int       `json:"position"`
	SourceFile string    `json:"source_file"`
}

func (f *FolderWatch) Export(ctx context.Context, task Task) (string, error) {
	if f.store == nil {
		return "", fmt.Errorf("folder_watch export needs object storage")
	}
	base := filepath.Join(f.root, folderOutgoing, task.ID.String())
	srcDir := filepath.Join(base, folderSource)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return "", fmt.Errorf("create outgoing dir: %w", err)
	}

	manifest := folderManifest{
		Format:    manifestFormat,
		TaskID:    task.ID,
		RoundID:   task.RoundID,
		RoundNo:   task.RoundNo,
		CreatedAt: time.Now().UTC(),
		Note: "Retouch the listed sources and drop JPEG/PNG/WebP files into " +
			filepath.Join(folderIncoming, task.ID.String(), "<asset_id>.jpg") + ". " +
			"File names are asset ids; the manifest maps them to original names.",
	}
	for _, item := range task.Items {
		if strings.TrimSpace(item.SourceKey) == "" {
			return "", fmt.Errorf("asset %s has no stored source", item.AssetID)
		}
		name := item.AssetID.String() + normalizeExt(item.Ext)
		if err := f.copyObject(ctx, item.SourceKey, srcDir, name); err != nil {
			return "", fmt.Errorf("export asset %s: %w", item.AssetID, err)
		}
		manifest.Items = append(manifest.Items, folderManifestItem{
			AssetID:    item.AssetID,
			Filename:   item.Filename,
			Position:   item.Position,
			SourceFile: filepath.ToSlash(filepath.Join(folderSource, name)),
		})
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode manifest: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(base, manifestName), raw); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	return filepath.ToSlash(filepath.Join(folderOutgoing, task.ID.String())), nil
}

// copyObject streams one stored object into the export directory. The temp file
// lives in the destination directory so the final rename stays on one volume.
func (f *FolderWatch) copyObject(ctx context.Context, key, dir, name string) error {
	rc, err := f.store.Open(ctx, key)
	if err != nil {
		return fmt.Errorf("open source %s: %w", key, err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(dir, ".export-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, name))
}

// Poll lists files the retoucher has placed under incoming/<taskID>/. Only
// UUID-named directories and UUID-named image files are considered, and done/
// or failed/ subdirectories are never re-read.
func (f *FolderWatch) Poll(context.Context) ([]Import, error) {
	base := filepath.Join(f.root, folderIncoming)
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read incoming dir: %w", err)
	}
	var out []Import
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskID, err := uuid.Parse(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(base, entry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read task incoming dir: %w", err)
		}
		for _, file := range files {
			if file.IsDir() || strings.HasPrefix(file.Name(), ".") {
				continue
			}
			assetID, ok := importAssetID(file.Name())
			if !ok {
				continue
			}
			info, err := file.Info()
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("stat incoming file: %w", err)
			}
			if info.Size() == 0 {
				continue
			}
			out = append(out, Import{
				TaskID:   taskID,
				AssetID:  assetID,
				Path:     filepath.Join(dir, file.Name()),
				Filename: file.Name(),
				Size:     info.Size(),
			})
		}
	}
	return out, nil
}

// Archive moves a processed file out of the scanned directory. ok=false keeps
// the rejected file for inspection under failed/.
func (f *FolderWatch) Archive(imp Import, ok bool) error {
	// Import paths come from Poll's own walk, but the destination must never
	// escape the configured root even if a caller hand-builds one.
	rel, err := filepath.Rel(f.root, imp.Path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refuse to archive %s outside the folder root", imp.Path)
	}
	sub := folderDone
	if !ok {
		sub = folderFailed
	}
	dir := filepath.Join(filepath.Dir(imp.Path), sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create archive dir: %w", err)
	}
	dst := filepath.Join(dir, filepath.Base(imp.Path))
	if _, err := os.Stat(dst); err == nil {
		dst = filepath.Join(dir, fmt.Sprintf("%s-%d%s",
			strings.TrimSuffix(filepath.Base(imp.Path), filepath.Ext(imp.Path)),
			time.Now().UnixNano(), filepath.Ext(imp.Path)))
	}
	if err := os.Rename(imp.Path, dst); err != nil {
		return fmt.Errorf("archive incoming file: %w", err)
	}
	return nil
}

var importExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
}

func importAssetID(name string) (uuid.UUID, bool) {
	ext := strings.ToLower(filepath.Ext(name))
	if !importExtensions[ext] {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(strings.TrimSuffix(name, filepath.Ext(name)))
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// normalizeExt keeps only a short alphanumeric suffix; the extension is part of
// a generated file name, never user-controlled.
func normalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ext, ".")))
	if ext == "" || len(ext) > 8 {
		return ".bin"
	}
	for _, r := range ext {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ".bin"
		}
	}
	return "." + ext
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
