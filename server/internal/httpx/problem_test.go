package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"valid", `{"title":"x"}`, ""},
		{"valid whitespace", "{\"title\":\"x\"}\n\t ", ""},
		{"empty", ``, "empty body"},
		{"trailing object", `{"title":"x"}{"title":"y"}`, "unexpected trailing data"},
		{"trailing garbage", `{"title":"x"} oops`, "unexpected trailing data"},
		{"malformed", `{"title":`, "invalid json"},
		{"oversized", `{"title":"` + strings.Repeat("a", 3<<20) + `"}`, "invalid json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
			rec := httptest.NewRecorder()
			var dst struct {
				Title string `json:"title"`
			}
			err := DecodeJSON(rec, req, &dst)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if dst.Title != "x" {
					t.Fatalf("title = %q", dst.Title)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}
