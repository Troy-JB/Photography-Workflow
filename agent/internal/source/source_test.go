package source

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDirSource(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "DCIM"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "DCIM", "a.txt"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := NewDir(root)
	if err != nil {
		t.Fatalf("new dir: %v", err)
	}
	if err := src.Alive(); err != nil {
		t.Fatalf("alive: %v", err)
	}
	info, err := src.FS().(interface {
		Stat(string) (os.FileInfo, error)
	}).Stat("DCIM/a.txt")
	if err != nil || info.Size() != 3 {
		t.Fatalf("stat = %+v, %v", info, err)
	}
	f, err := src.FS().Open("DCIM/a.txt")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "abc" {
		t.Fatalf("content = %q", b)
	}
	if _, err := src.FS().Open("../escape"); err == nil {
		t.Fatal("path escape should be rejected")
	}
}

func TestNewDirRejectsMissing(t *testing.T) {
	if _, err := NewDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing root should fail")
	}
}
