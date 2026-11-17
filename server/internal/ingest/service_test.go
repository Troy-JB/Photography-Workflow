package ingest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const validSHA = "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12"

func TestNormalizeFile(t *testing.T) {
	in := FileInput{
		RelPath: `100CANON\IMG_0001.CR3`,
		Size:    48234496,
		SHA256:  strings.ToUpper(validSHA),
		Exif:    json.RawMessage(`{"iso":400}`),
	}
	f, err := normalizeFile(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if f.RelPath != "100CANON/IMG_0001.CR3" {
		t.Fatalf("rel_path = %q", f.RelPath)
	}
	if f.SHA256 != validSHA {
		t.Fatalf("sha256 = %q, want lowercase", f.SHA256)
	}

	in.Exif = json.RawMessage(`null`)
	f, err = normalizeFile(in)
	if err != nil {
		t.Fatalf("normalize null exif: %v", err)
	}
	if f.Exif != nil {
		t.Fatalf("null exif should normalize to nil, got %s", f.Exif)
	}
}

func TestNormalizeFileRejectsBadInput(t *testing.T) {
	base := FileInput{RelPath: "a.jpg", Size: 1, SHA256: validSHA}
	cases := []struct {
		name string
		mod  func(*FileInput)
	}{
		{"traversal", func(f *FileInput) { f.RelPath = "../etc/passwd" }},
		{"parent only", func(f *FileInput) { f.RelPath = ".." }},
		{"nested traversal", func(f *FileInput) { f.RelPath = "a/../../b.jpg" }},
		{"absolute", func(f *FileInput) { f.RelPath = "/etc/passwd" }},
		{"drive letter", func(f *FileInput) { f.RelPath = `C:\DCIM\a.jpg` }},
		{"empty", func(f *FileInput) { f.RelPath = "" }},
		{"short hash", func(f *FileInput) { f.SHA256 = "abcd" }},
		{"non hex hash", func(f *FileInput) { f.SHA256 = strings.Repeat("zz", 32) }},
		{"negative size", func(f *FileInput) { f.Size = -1 }},
		{"bad exif", func(f *FileInput) { f.Exif = json.RawMessage(`{`) }},
	}
	for _, c := range cases {
		in := base
		c.mod(&in)
		if _, err := normalizeFile(in); !errors.Is(err, ErrInvalidFile) {
			t.Fatalf("%s: expected ErrInvalidFile, got %v", c.name, err)
		}
	}
}

func TestObjectKeyIsContentAddressed(t *testing.T) {
	tenant, project := uuid.New(), uuid.New()
	key := objectKey(tenant, project, validSHA, "IMG_0001.CR3")
	want := "t/" + tenant.String() + "/p/" + project.String() + "/ab/" + validSHA + "/original.cr3"
	if key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
}

func TestChoosePartSize(t *testing.T) {
	ps, err := choosePartSize(33<<20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ps != DefaultPartSize {
		t.Fatalf("default part size = %d", ps)
	}

	ps, err = choosePartSize(1<<30, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ps != MinPartSize {
		t.Fatalf("requested part size should clamp to minimum, got %d", ps)
	}

	ps, err = choosePartSize(100<<30, 5<<20)
	if err != nil {
		t.Fatal(err)
	}
	if parts := (int64(100<<30) + ps - 1) / ps; parts > MaxParts {
		t.Fatalf("part size %d still yields %d parts", ps, parts)
	}

	if _, err := choosePartSize(100<<40, 5<<20); err == nil {
		t.Fatal("expected too-large object to be rejected")
	}
}
