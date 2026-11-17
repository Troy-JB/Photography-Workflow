package gallery

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"photography-workflow/server/internal/storage"
)

// stubStore only serves Open; every other Storage method is intentionally
// unimplemented so an accidental call fails the test loudly.
type stubStore struct {
	storage.Storage
	objects map[string][]byte
	opens   int
}

func (s *stubStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.opens++
	data, ok := s.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestCursorRoundTrip(t *testing.T) {
	for _, position := range []int{0, 1, 39, 2048} {
		cursor := encodeCursor(position)
		got, err := decodeCursor(cursor)
		if err != nil {
			t.Fatalf("decode(%q): %v", cursor, err)
		}
		if got != position {
			t.Fatalf("round trip %d = %d", position, got)
		}
	}
	if _, err := decodeCursor("!!!not-base64!!!"); err == nil {
		t.Fatal("expected malformed cursor to fail")
	}
	if _, err := decodeCursor("YWJj"); err == nil {
		t.Fatal("expected non-numeric cursor to fail")
	}
}

func TestManifestCacheIsKeyedByObjectKey(t *testing.T) {
	store := &stubStore{objects: map[string][]byte{
		"t/x/galleries/g/v1/manifest.json": []byte(`{"manifest_version":1,"assets":[]}`),
		"t/x/galleries/g/v2/manifest.json": []byte(`{"manifest_version":2,"assets":[]}`),
	}}
	loader := NewManifestLoader(store)
	ctx := context.Background()

	first, err := loader.Load(ctx, "t/x/galleries/g/v1/manifest.json")
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	second, err := loader.Load(ctx, "t/x/galleries/g/v2/manifest.json")
	if err != nil {
		t.Fatalf("load v2: %v", err)
	}
	again, err := loader.Load(ctx, "t/x/galleries/g/v1/manifest.json")
	if err != nil {
		t.Fatalf("reload v1: %v", err)
	}
	if first.ManifestVersion != 1 || second.ManifestVersion != 2 || again.ManifestVersion != 1 {
		t.Fatalf("versions = %d/%d/%d", first.ManifestVersion, second.ManifestVersion, again.ManifestVersion)
	}
	if store.opens != 2 {
		t.Fatalf("object opened %d times, want 2 (one per key)", store.opens)
	}
}

func TestManifestCacheExpires(t *testing.T) {
	store := &stubStore{objects: map[string][]byte{
		"k": []byte(`{"manifest_version":1,"assets":[]}`),
	}}
	loader := NewManifestLoader(store)
	if _, err := loader.Load(context.Background(), "k"); err != nil {
		t.Fatalf("first load: %v", err)
	}
	loader.mu.Lock()
	entry := loader.manifests["k"]
	entry.at = time.Now().Add(-manifestCacheTTL - time.Second)
	loader.manifests["k"] = entry
	loader.mu.Unlock()

	if _, err := loader.Load(context.Background(), "k"); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if store.opens != 2 {
		t.Fatalf("object opened %d times, want 2 after TTL expiry", store.opens)
	}
}

func TestManifestMissingObjectIsAnError(t *testing.T) {
	loader := NewManifestLoader(&stubStore{objects: map[string][]byte{}})
	if _, err := loader.Load(context.Background(), "absent"); err == nil {
		t.Fatal("expected missing manifest to fail")
	}
	if _, err := loader.Load(context.Background(), ""); err == nil {
		t.Fatal("expected empty manifest key to fail")
	}
}
