//go:build windows

package core

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"photography-workflow/agent/internal/fsx"
	"photography-workflow/agent/internal/model"
)

// TestImportLongPathsWindows keeps a destination path past MAX_PATH so the
// \\?\ handling in fsx.Local is exercised by a real import, not just a unit
// test of the prefix helper.
func TestImportLongPathsWindows(t *testing.T) {
	e := newEnv(t, false)
	e.b = t.TempDir()
	e.src = t.TempDir()

	parts := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		parts = append(parts, fmt.Sprintf("%s%02d", strings.Repeat("long-segment-", 2), i))
	}
	rel := strings.Join(parts, "/") + "/IMG_LONG_0001.JPG"
	body := "long-path-bytes"
	e.files = map[string]string{rel: body}
	e.writeSource(t)

	im := e.start(t)
	res, err := e.imp.Run(context.Background(), im.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Paused || res.LocalVerified != 1 {
		t.Fatalf("result = %+v", res)
	}
	got, err := e.st.Import(context.Background(), im.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ImportLocalComplete {
		t.Fatalf("status = %q", got.Status)
	}

	for _, root := range []string{e.a, e.b} {
		final, err := findFile(root, "IMG_LONG_0001.JPG")
		if err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		if len(final) <= 260 {
			t.Fatalf("destination path is not long enough to prove anything: %d chars", len(final))
		}
		data, err := os.ReadFile(fsx.Local(final))
		if err != nil {
			t.Fatalf("read %s: %v", final, err)
		}
		if string(data) != body {
			t.Fatalf("copy content = %q, want %q", data, body)
		}
	}
}

func findFile(root, name string) (string, error) {
	var found string
	err := filepath.WalkDir(fsx.Local(root), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			found = strings.TrimPrefix(p, `\\?\`)
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("%s not found under %s", name, root)
	}
	return found, nil
}
