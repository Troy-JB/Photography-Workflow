package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"photography-workflow/agent/internal/model"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverClassifiesAndSkips(t *testing.T) {
	root := t.TempDir()
	write(t, root, "DCIM/100CANON/IMG_0001.CR3", "raw")
	write(t, root, "DCIM/100CANON/IMG_0001.JPG", "jpg")
	write(t, root, "DCIM/100CANON/IMG_0001.XMP", "xmp")
	write(t, root, "DCIM/100CANON/VID_0002.MP4", "mp4")
	write(t, root, "DCIM/100CANON/VID_0002.SRT", "srt")
	write(t, root, "DCIM/100CANON/UNKNOWN.LRV", "proxy")
	write(t, root, "DCIM/Thumbs.db", "junk")
	write(t, root, "DCIM/._IMG_0001.CR3", "resource fork")
	write(t, root, "DCIM/.DS_Store", "junk")
	write(t, root, "$RECYCLE.BIN/S-1-5-21/x.dat", "junk")
	write(t, root, "System Volume Information/tracking.log", "junk")

	entries, err := Discover(context.Background(), os.DirFS(root))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("entries = %d: %+v", len(entries), entries)
	}
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.RelPath] = e
	}
	raw := byPath["DCIM/100CANON/IMG_0001.CR3"]
	jpg := byPath["DCIM/100CANON/IMG_0001.JPG"]
	xmp := byPath["DCIM/100CANON/IMG_0001.XMP"]
	vid := byPath["DCIM/100CANON/VID_0002.MP4"]
	srt := byPath["DCIM/100CANON/VID_0002.SRT"]
	proxy := byPath["DCIM/100CANON/UNKNOWN.LRV"]

	if raw.Kind != model.KindPhoto || raw.Role != model.RoleRAW {
		t.Fatalf("raw = %+v", raw)
	}
	if jpg.Role != model.RoleJPEG || xmp.Role != model.RoleSidecar {
		t.Fatalf("jpg/xmp roles = %v/%v", jpg.Role, xmp.Role)
	}
	if vid.Kind != model.KindVideo || srt.Kind != model.KindSidecar {
		t.Fatalf("video = %+v srt = %+v", vid, srt)
	}
	if proxy.Kind != model.KindOther {
		t.Fatalf("unknown extension should be copied as other: %+v", proxy)
	}
	if raw.PairKey != jpg.PairKey || raw.PairKey != xmp.PairKey {
		t.Fatalf("pair keys differ: %q %q %q", raw.PairKey, jpg.PairKey, xmp.PairKey)
	}
	if raw.Size != 3 || raw.MtimeNS == 0 {
		t.Fatalf("size/mtime = %d/%d", raw.Size, raw.MtimeNS)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].RelPath >= entries[i].RelPath {
			t.Fatalf("entries not sorted at %d", i)
		}
	}
}

func TestDiscoverCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, os.DirFS(t.TempDir())); err == nil {
		t.Fatal("cancelled discovery should fail")
	}
}
