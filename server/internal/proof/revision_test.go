package proof

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"photography-workflow/server/internal/storage"
)

// deleteRecorder records Delete calls; the embedded nil interface panics if any
// other storage method is reached, which keeps the test focused.
type deleteRecorder struct {
	storage.Storage
	keys []string
}

func (d *deleteRecorder) Delete(_ context.Context, key string) error {
	d.keys = append(d.keys, key)
	return nil
}

func TestDiscardRevisionSource(t *testing.T) {
	store := &deleteRecorder{}
	svc := &Service{store: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	svc.discardRevisionSource(context.Background(), "")
	if len(store.keys) != 0 {
		t.Fatalf("empty key deleted %v, want no calls", store.keys)
	}

	svc.discardRevisionSource(context.Background(), "t/tenant/revisions/asset/v2/source.jpg")
	if len(store.keys) != 1 || store.keys[0] != "t/tenant/revisions/asset/v2/source.jpg" {
		t.Fatalf("deleted %v, want the stored revision source", store.keys)
	}
}

func TestNormalizeSHA256(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	got, err := normalizeSHA256(strings.ToUpper(valid))
	if err != nil {
		t.Fatalf("normalize valid: %v", err)
	}
	if got != valid {
		t.Fatalf("normalized = %s, want %s", got, valid)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 64)} {
		if _, err := normalizeSHA256(bad); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

func TestSpoolRevisionDetectsFormat(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		ext  string
		mime string
	}{
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}, "jpg", "image/jpeg"},
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}, "png", "image/png"},
		{"webp", []byte{'R', 'I', 'F', 'F', 0x10, 0, 0, 0, 'W', 'E', 'B', 'P'}, "webp", "image/webp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spool, err := spoolRevision(bytes.NewReader(tc.head), 1024)
			if err != nil {
				t.Fatalf("spool: %v", err)
			}
			defer spool.close()
			if spool.ext != tc.ext || spool.contentType != tc.mime {
				t.Fatalf("sniffed %s/%s, want %s/%s", spool.ext, spool.contentType, tc.ext, tc.mime)
			}
			if spool.size != int64(len(tc.head)) {
				t.Fatalf("size = %d, want %d", spool.size, len(tc.head))
			}
			if len(spool.sha) != 64 {
				t.Fatalf("sha = %q", spool.sha)
			}
		})
	}
}

func TestSpoolRevisionRejectsUnsupportedAndOversized(t *testing.T) {
	// TIFF header stands in for a RAW/TIFF upload that must never reach storage.
	if _, err := spoolRevision(bytes.NewReader([]byte{'I', 'I', 0x2A, 0, 8, 0, 0, 0, 0, 0, 0, 0}), 1024); !errors.Is(err, ErrRevisionFormat) {
		t.Fatalf("expected format error, got %v", err)
	}
	if _, err := spoolRevision(bytes.NewReader(nil), 1024); err == nil {
		t.Fatal("expected empty upload to be rejected")
	}
	big := bytes.Repeat([]byte{0xFF}, 64)
	if _, err := spoolRevision(bytes.NewReader(big), 32); !errors.Is(err, ErrRevisionTooLarge) {
		t.Fatalf("expected too-large error, got %v", err)
	}
}

func TestValidateComment(t *testing.T) {
	annotation := []byte(`{"schema":"annotation.v1","type":"point","points":[{"x":0.25,"y":0.5}]}`)

	body, raw, ref, err := validateComment(CommentInput{
		Body: "  请把天空调亮  ", Annotation: annotation, ClientRef: " mobile-1 ",
	}, 16384)
	if err != nil {
		t.Fatalf("valid comment: %v", err)
	}
	if body != "请把天空调亮" || ref != "mobile-1" || raw == nil {
		t.Fatalf("normalised body=%q ref=%q annotation=%s", body, ref, raw)
	}

	if _, _, _, err := validateComment(CommentInput{Body: strings.Repeat("x", MaxCommentBody+1)}, 16384); !errors.Is(err, ErrCommentLimit) {
		t.Fatalf("expected limit error, got %v", err)
	}
	if _, _, _, err := validateComment(CommentInput{}, 16384); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected empty comment error, got %v", err)
	}
	if _, _, _, err := validateComment(CommentInput{Annotation: []byte(`{"schema":"annotation.v1","type":"point","points":[]}`)}, 16384); !errors.Is(err, ErrInvalidAnnotation) {
		t.Fatalf("expected annotation error, got %v", err)
	}
	if _, _, _, err := validateComment(CommentInput{Body: "hi", ClientRef: strings.Repeat("r", maxClientRefLen+1)}, 16384); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected client_ref error, got %v", err)
	}
}
