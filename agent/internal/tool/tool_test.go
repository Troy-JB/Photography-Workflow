package tool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocatePrefersExplicitPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "custom-exiftool")
	if err := os.WriteFile(exe, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Locate(exe, "")
	abs, err := filepath.Abs(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExifTool != abs {
		t.Fatalf("ExifTool = %q, want %q", got.ExifTool, abs)
	}
	if !got.HasExifTool() {
		t.Fatal("HasExifTool should be true")
	}
}

func TestLocateIgnoresMissingExplicitPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.exe")
	got := Locate(missing, missing)
	if got.ExifTool == missing || got.FFmpeg == missing {
		t.Fatalf("missing explicit paths must not be returned: %+v", got)
	}
}

func TestLocateIgnoresDirectory(t *testing.T) {
	dir := t.TempDir()
	got := Locate(dir, dir)
	if got.ExifTool == dir || got.FFmpeg == dir {
		t.Fatalf("directories must not be treated as binaries: %+v", got)
	}
}
