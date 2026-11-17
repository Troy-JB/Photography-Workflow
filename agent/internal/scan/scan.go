// Package scan discovers card content: it skips operating-system noise,
// classifies files by extension and pairs RAW+JPEG siblings for preview use.
package scan

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"photography-workflow/agent/internal/model"
)

// Entry is one file found on the card, before any copy work.
type Entry struct {
	RelPath string
	Size    int64
	MtimeNS int64
	Kind    model.FileKind
	Role    model.FileRole
	PairKey string
}

var rawExt = map[string]bool{
	".cr2": true, ".cr3": true, ".nef": true, ".nrw": true, ".arw": true,
	".srf": true, ".sr2": true, ".raf": true, ".orf": true, ".rw2": true,
	".rwl": true, ".dng": true, ".pef": true, ".srw": true, ".raw": true,
	".3fr": true, ".fff": true, ".iiq": true, ".mos": true, ".mrw": true,
	".x3f": true, ".erf": true, ".kdc": true, ".dcr": true, ".mef": true,
}

var imageExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".tif": true, ".tiff": true,
	".heic": true, ".heif": true, ".avif": true, ".webp": true, ".bmp": true,
	".gif": true,
}

var videoExt = map[string]bool{
	".mp4": true, ".mov": true, ".m4v": true, ".avi": true, ".mkv": true,
	".mpg": true, ".mpeg": true, ".mts": true, ".m2ts": true, ".3gp": true,
	".wmv": true, ".webm": true,
}

var sidecarExt = map[string]bool{
	".xmp": true, ".aae": true, ".dop": true, ".pp3": true, ".on1": true,
	".srt": true, ".wav": true,
}

var skipNames = map[string]bool{
	"thumbs.db":   true,
	"desktop.ini": true,
	".ds_store":   true,
}

var skipDirs = map[string]bool{
	"$recycle.bin":              true,
	"system volume information": true,
	".trashes":                  true,
	".spotlight-v100":           true,
	".fseventsd":                true,
	".temporaryitems":           true,
}

// Classify returns the media kind and role for a slash-separated path.
func Classify(rel string) (model.FileKind, model.FileRole) {
	ext := strings.ToLower(path.Ext(rel))
	switch {
	case rawExt[ext]:
		return model.KindPhoto, model.RoleRAW
	case imageExt[ext]:
		if ext == ".jpg" || ext == ".jpeg" {
			return model.KindPhoto, model.RoleJPEG
		}
		return model.KindPhoto, model.RoleImage
	case videoExt[ext]:
		return model.KindVideo, model.RoleVideo
	case sidecarExt[ext]:
		return model.KindSidecar, model.RoleSidecar
	}
	return model.KindOther, model.RoleOther
}

// PairKey strips the extension so RAW and JPEG siblings of the same shot share
// a key. It only influences preview selection, never copy or dedupe.
func PairKey(rel string) string {
	ext := path.Ext(rel)
	if ext == "" {
		return rel
	}
	return rel[:len(rel)-len(ext)]
}

// Skip reports whether a file name is OS noise that must never be copied.
func Skip(name string) bool {
	lower := strings.ToLower(name)
	if skipNames[lower] {
		return true
	}
	// AppleDouble resource forks and Finder metadata.
	if strings.HasPrefix(name, "._") {
		return true
	}
	return false
}

func skipDir(name string) bool {
	return skipDirs[strings.ToLower(name)]
}

// Discover walks the source and returns entries sorted by relative path.
func Discover(ctx context.Context, fsys fs.FS) ([]Entry, error) {
	var entries []Entry
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			return fmt.Errorf("walk %s: %w", p, err)
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks, devices and sockets are not card content; following
			// them could escape the card or loop.
			return nil
		}
		if Skip(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		kind, role := Classify(p)
		entries = append(entries, Entry{
			RelPath: p,
			Size:    info.Size(),
			MtimeNS: info.ModTime().UnixNano(),
			Kind:    kind,
			Role:    role,
			PairKey: PairKey(p),
		})
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelPath < entries[j].RelPath })
	return entries, nil
}
