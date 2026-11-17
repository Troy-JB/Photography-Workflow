// Package card detects removable camera volumes and resolves an import's card
// back to its current mount point. Identity is the volume serial, capacity and
// label, never the drive letter.
package card

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/source"
)

type Volume struct {
	Root     string // current mount point, e.g. `E:\`
	GUID     string // `\\?\Volume{...}\`
	Label    string
	Serial   uint32
	Capacity uint64
}

// Fingerprint is stable across re-insertion and ignores the drive letter.
func (v Volume) Fingerprint() string {
	return fmt.Sprintf("vol:%08X:%d:%s", v.Serial, v.Capacity, v.Label)
}

func (v Volume) Model() model.Card {
	return model.Card{
		Fingerprint: v.Fingerprint(),
		VolumeGUID:  v.GUID,
		Label:       v.Label,
		Capacity:    int64(v.Capacity),
	}
}

func (v Volume) Source() (source.Source, error) {
	return source.NewDir(v.Root)
}

// DisplayLabel is the operator-facing name for a card.
func DisplayLabel(label string) string {
	if label == "" {
		return "未命名存储卡"
	}
	return label
}

// ForRoot returns the volume identity for a mount point, or a zero Card when
// root is a plain directory (manual folder imports keep a dir fingerprint).
func ForRoot(ctx context.Context, root string) model.Card {
	vols, err := Detect(ctx)
	if err != nil {
		return model.Card{}
	}
	for _, v := range vols {
		if strings.EqualFold(filepath.Clean(v.Root), filepath.Clean(root)) {
			return v.Model()
		}
	}
	return model.Card{}
}

// CardStore is the store slice the resolver needs.
type CardStore interface {
	Card(ctx context.Context, id string) (*model.Card, error)
}

// Resolver maps a persisted card (by volume GUID) to its current mount point,
// falling back to the recorded source root for directory imports.
type Resolver struct {
	Store CardStore
}

func (r Resolver) Resolve(ctx context.Context, imp *model.Import) (source.Source, error) {
	if r.Store != nil {
		c, err := r.Store.Card(ctx, imp.CardID)
		if err != nil {
			return nil, err
		}
		if c.VolumeGUID != "" {
			root, err := MountPoint(c.VolumeGUID)
			if err != nil {
				return nil, fmt.Errorf("card volume %s is not mounted: %w", c.VolumeGUID, err)
			}
			return source.NewDir(root)
		}
	}
	return source.NewDir(imp.SourceRoot)
}

// SourceRootFor is a helper for callers that only need the display path.
func SourceRootFor(c *model.Card, fallback string) string {
	if c == nil || c.VolumeGUID == "" {
		return fallback
	}
	if root, err := MountPoint(c.VolumeGUID); err == nil {
		return filepath.Clean(root)
	}
	return fallback
}
