package gallery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/storage"
)

const (
	maxManifestBytes = 32 << 20
	manifestCacheTTL = 2 * time.Minute
)

// Manifest is the immutable description of a published gallery. It lives in
// object storage and never contains URLs: the customer API signs short-lived
// links from the object keys, so a leaked manifest cannot serve images.
type Manifest struct {
	ManifestVersion int             `json:"manifest_version"`
	GalleryID       uuid.UUID       `json:"gallery_id"`
	ProjectID       uuid.UUID       `json:"project_id"`
	Title           string          `json:"title"`
	RoundNo         int             `json:"round_no"`
	SelectionLimit  *int            `json:"selection_limit"`
	DownloadAllowed bool            `json:"download_allowed"`
	ExpiresAt       *time.Time      `json:"expires_at,omitempty"`
	Watermark       string          `json:"watermark,omitempty"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Assets          []ManifestAsset `json:"assets"`
}

// ManifestAsset is one entry of a published gallery. Keys are storage keys,
// never public URLs.
type ManifestAsset struct {
	AssetID       uuid.UUID `json:"asset_id"`
	Filename      string    `json:"filename"`
	Position      int       `json:"position"`
	ContentHash   string    `json:"content_hash"`
	ThumbKey      string    `json:"thumb_key"`
	ThumbWidth    int       `json:"thumb_width"`
	ThumbHeight   int       `json:"thumb_height"`
	DisplayKey    string    `json:"display_key"`
	DisplayWidth  int       `json:"display_width"`
	DisplayHeight int       `json:"display_height"`
	LQIP          string    `json:"lqip,omitempty"`
	Color         string    `json:"color,omitempty"`
	IsVideo       bool      `json:"is_video"`
}

// ManifestObjectKey is versioned per publish round so an old manifest stays
// readable after a reopen/re-publish.
func ManifestObjectKey(tenantID, galleryID uuid.UUID, version int) string {
	return fmt.Sprintf("t/%s/galleries/%s/v%d/manifest.json", tenantID, galleryID, version)
}

// ManifestLoader reads immutable manifests with a small in-process cache keyed
// by the versioned object key, so republishing a round never serves a stale
// document. Both the selection and proof customer APIs share it.
type ManifestLoader struct {
	store storage.Storage

	mu        sync.Mutex
	manifests map[string]cachedManifest
}

type cachedManifest struct {
	manifest *Manifest
	at       time.Time
}

func NewManifestLoader(store storage.Storage) *ManifestLoader {
	return &ManifestLoader{store: store, manifests: make(map[string]cachedManifest)}
}

func (l *ManifestLoader) Load(ctx context.Context, key string) (*Manifest, error) {
	if key == "" {
		return nil, fmt.Errorf("gallery has no manifest")
	}
	l.mu.Lock()
	if entry, ok := l.manifests[key]; ok && time.Since(entry.at) < manifestCacheTTL {
		l.mu.Unlock()
		return entry.manifest, nil
	}
	l.mu.Unlock()

	rc, err := l.store.Open(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("manifest object %s is missing", key)
		}
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer rc.Close()
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(rc, maxManifestBytes)).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	l.mu.Lock()
	if len(l.manifests) >= 8 {
		for k := range l.manifests {
			delete(l.manifests, k)
		}
	}
	l.manifests[key] = cachedManifest{manifest: &m, at: time.Now()}
	l.mu.Unlock()
	return &m, nil
}

func buildManifest(g *Gallery, watermark string, rows []manifestRow, now time.Time) *Manifest {
	m := &Manifest{
		ManifestVersion: g.ManifestVersion,
		GalleryID:       g.ID,
		ProjectID:       g.ProjectID,
		Title:           g.Title,
		RoundNo:         g.RoundNo,
		SelectionLimit:  g.SelectionLimit,
		DownloadAllowed: g.DownloadAllowed,
		ExpiresAt:       g.ExpiresAt,
		Watermark:       watermark,
		GeneratedAt:     now.UTC(),
		Assets:          make([]ManifestAsset, 0, len(rows)),
	}
	for _, r := range rows {
		m.Assets = append(m.Assets, ManifestAsset{
			AssetID:       r.AssetID,
			Filename:      r.Filename,
			Position:      r.Position,
			ContentHash:   r.ContentHash,
			ThumbKey:      r.ThumbKey,
			ThumbWidth:    r.ThumbWidth,
			ThumbHeight:   r.ThumbHeight,
			DisplayKey:    r.DisplayKey,
			DisplayWidth:  r.DisplayWidth,
			DisplayHeight: r.DisplayHeight,
			LQIP:          r.LQIP,
			Color:         r.Color,
			IsVideo:       r.IsVideo,
		})
	}
	return m
}
