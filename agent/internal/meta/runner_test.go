package meta

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildFake compiles testdata/fakeexiftool so the protocol is exercised with
// a real subprocess on every platform.
func buildFake(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	name := "fakeexiftool"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fakeexiftool")
	cmd.Dir = dir
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake exiftool: %v\n%s", err, combined)
	}
	return out
}

func TestStayOpenRoundTrip(t *testing.T) {
	r, err := Open(buildFake(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	for i := 0; i < 3; i++ {
		data, err := r.JSON(context.Background(), "C:/cards/IMG.CR3")
		if err != nil {
			t.Fatalf("json %d: %v", i, err)
		}
		if !strings.Contains(string(data), `"EXIF:Make":"Canon"`) {
			t.Fatalf("json %d = %s", i, data)
		}
		md, err := Parse(data, "IMG.CR3")
		if err != nil {
			t.Fatalf("parse %d: %v", i, err)
		}
		if md.Camera != "Canon EOS R5" || md.Width != 6000 {
			t.Fatalf("metadata %d = %+v", i, md)
		}
	}

	preview, err := r.Binary(context.Background(), "C:/cards/IMG.CR3", PreviewTags)
	if err != nil {
		t.Fatalf("binary: %v", err)
	}
	if !IsJPEG(preview) {
		t.Fatalf("binary preview is not JPEG: %x", preview)
	}
}

func TestStayOpenRestartsAfterTimeout(t *testing.T) {
	r, err := Open(buildFake(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := r.JSON(ctx, "hang"); err == nil {
		t.Fatal("hanging extraction should fail")
	}

	// The reader must recover by starting a fresh process.
	data, err := r.JSON(context.Background(), "C:/cards/IMG.CR3")
	if err != nil {
		t.Fatalf("after timeout: %v", err)
	}
	if !strings.Contains(string(data), "Canon") {
		t.Fatalf("after timeout = %s", data)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open("  "); err == nil {
		t.Fatal("empty path must fail")
	}
}
