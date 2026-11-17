package hash

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := File(context.Background(), path)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got.SHA256 != want || got.Bytes != 5 {
		t.Fatalf("result = %+v", got)
	}
}

func TestReaderCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Reader(ctx, strings.NewReader("x")); err == nil {
		t.Fatal("cancelled context should abort")
	}
}
