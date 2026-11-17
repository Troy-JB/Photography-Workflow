package storage

import (
	"net/url"
	"strings"
	"testing"
)

func TestContentDisposition(t *testing.T) {
	if got := contentDisposition("PWF-001_web_20260304.zip"); got != `attachment; filename="PWF-001_web_20260304.zip"` {
		t.Fatalf("ascii = %q", got)
	}
	if got := contentDisposition(""); got != `attachment; filename="download"` {
		t.Fatalf("empty = %q", got)
	}
	if got := contentDisposition("a\r\nb.zip"); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("header injection survived: %q", got)
	}
	if got := contentDisposition(`a"b.zip`); !strings.Contains(got, `filename*=UTF-8''`) {
		t.Fatalf("quote should force the extended form: %q", got)
	}

	got := contentDisposition("交付_2026.zip")
	if !strings.HasPrefix(got, `attachment; filename="download"; filename*=UTF-8''`) {
		t.Fatalf("cjk = %q", got)
	}
	encoded := strings.TrimPrefix(got, `attachment; filename="download"; filename*=UTF-8''`)
	decoded, err := url.QueryUnescape(encoded)
	if err != nil {
		t.Fatalf("unescape %q: %v", encoded, err)
	}
	if decoded != "交付_2026.zip" {
		t.Fatalf("decoded = %q", decoded)
	}
	if strings.Contains(encoded, "+") {
		t.Fatalf("RFC 5987 value must not use '+' for spaces: %q", encoded)
	}
}
