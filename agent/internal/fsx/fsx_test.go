package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeSegment(t *testing.T) {
	cases := []struct {
		in, fallback, want string
	}{
		{"  PRJ-2026-001  ", "x", "PRJ-2026-001"},
		{"Client: A/B", "x", "Client_ A_B"},
		{"..", "x", "x"},
		{"", "x", "x"},
		{"CON", "x", "_CON"},
		{"com1", "x", "_com1"},
		{"CARD_A.", "x", "CARD_A"},
		{strings.Repeat("长", 100), "x", strings.Repeat("长", 26)},
	}
	for _, c := range cases {
		if got := SanitizeSegment(c.in, c.fallback); got != c.want {
			t.Errorf("SanitizeSegment(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCleanRel(t *testing.T) {
	good := map[string]string{
		"DCIM/100CANON/IMG_0001.CR3": "DCIM/100CANON/IMG_0001.CR3",
		`DCIM\100CANON\IMG_0001.JPG`: "DCIM/100CANON/IMG_0001.JPG",
		"file.txt":                   "file.txt",
	}
	for in, want := range good {
		got, err := CleanRel(in)
		if err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "/abs", "a/../../b", `C:\x`, "a//b"} {
		if _, err := CleanRel(bad); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanRel(%q) err = %v, want ErrInvalidPath", bad, err)
		}
	}
}

func TestWithin(t *testing.T) {
	base := filepath.Join("root", "a")
	if !Within(base, filepath.Join("root", "a", "b", "c")) {
		t.Fatal("child should be within")
	}
	if Within(base, filepath.Join("root", "ab")) {
		t.Fatal("sibling with shared prefix is not within")
	}
	if Within(base, filepath.Join("root", "a", "..", "b")) {
		t.Fatal("escape is not within")
	}
	if !Within(base, base) {
		t.Fatal("base is within itself")
	}
}

func TestRenameNoReplace(t *testing.T) {
	dir := t.TempDir()
	old1 := filepath.Join(dir, "one.part")
	final := filepath.Join(dir, "one.jpg")
	if err := os.WriteFile(old1, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(old1, final); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if b, err := os.ReadFile(final); err != nil || string(b) != "payload" {
		t.Fatalf("final = %q, %v", b, err)
	}
	if _, err := os.Stat(old1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp still present: %v", err)
	}

	old2 := filepath.Join(dir, "two.part")
	if err := os.WriteFile(old2, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := RenameNoReplace(old2, final)
	if !errors.Is(err, ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	if b, _ := os.ReadFile(final); string(b) != "payload" {
		t.Fatalf("existing destination was replaced: %q", b)
	}
	if b, _ := os.ReadFile(old2); string(b) != "other" {
		t.Fatalf("source was moved despite conflict: %q", b)
	}
}

func TestExistsAndRemove(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	ok, err := Exists(missing)
	if err != nil || ok {
		t.Fatalf("Exists(missing) = %v, %v", ok, err)
	}
	if err := EnsureDir(filepath.Join(dir, "deep", "nested")); err != nil {
		t.Fatalf("ensure dir: %v", err)
	}
	if err := RemoveIfExists(missing); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
}

func TestFreeSpace(t *testing.T) {
	free, err := FreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("free space: %v", err)
	}
	if free == 0 {
		t.Fatal("free space reported as zero on a writable temp dir")
	}
}
