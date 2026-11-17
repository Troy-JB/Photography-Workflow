package card

import (
	"context"
	"path/filepath"
	"testing"

	"photography-workflow/agent/internal/model"
)

func TestVolumeFingerprintIgnoresMountPoint(t *testing.T) {
	base := Volume{Root: `E:\`, GUID: `\\?\Volume{one}\`, Label: "CARD_A", Serial: 0x1A2B3C4D, Capacity: 64 << 30}
	moved := base
	moved.Root = `F:\`
	if base.Fingerprint() != moved.Fingerprint() {
		t.Fatalf("fingerprint depends on drive letter: %q != %q", base.Fingerprint(), moved.Fingerprint())
	}
	other := base
	other.Label = "CARD_B"
	if base.Fingerprint() == other.Fingerprint() {
		t.Fatal("label must participate in the fingerprint")
	}
	other = base
	other.Capacity++
	if base.Fingerprint() == other.Fingerprint() {
		t.Fatal("capacity must participate in the fingerprint")
	}
}

func TestVolumeModel(t *testing.T) {
	v := Volume{Root: `E:\`, GUID: `\\?\Volume{x}\`, Label: "CARD_A", Serial: 7, Capacity: 128 << 30}
	m := v.Model()
	if m.Fingerprint != v.Fingerprint() || m.VolumeGUID != v.GUID || m.Label != v.Label {
		t.Fatalf("model = %+v", m)
	}
	if m.Capacity != int64(128<<30) {
		t.Fatalf("capacity = %d", m.Capacity)
	}
}

type fakeStore struct {
	card *model.Card
	err  error
}

func (f fakeStore) Card(context.Context, string) (*model.Card, error) { return f.card, f.err }

func TestResolverFallsBackToSourceRoot(t *testing.T) {
	dir := t.TempDir()
	r := Resolver{Store: fakeStore{card: &model.Card{ID: "c1"}}}
	src, err := r.Resolve(context.Background(), &model.Import{CardID: "c1", SourceRoot: dir})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !filepath.IsAbs(src.Root()) || src.Root() != dir {
		t.Fatalf("root = %q", src.Root())
	}
}

func TestDisplayLabel(t *testing.T) {
	if DisplayLabel("") == "" || DisplayLabel("CARD") != "CARD" {
		t.Fatal("display label")
	}
}
