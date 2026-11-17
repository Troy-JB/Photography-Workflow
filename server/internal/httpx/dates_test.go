package httpx

import (
	"testing"
	"time"
)

func TestParseOptionalDate(t *testing.T) {
	if got, err := ParseOptionalDate("  "); err != nil || got != nil {
		t.Fatalf("empty = %v, %v", got, err)
	}
	got, err := ParseOptionalDate("2026-10-29")
	if err != nil || got == nil || got.Format("2006-01-02") != "2026-10-29" {
		t.Fatalf("date = %v, %v", got, err)
	}
	got, err = ParseOptionalDate("2026-10-29T10:30:00Z")
	if err != nil || got == nil || !got.Equal(time.Date(2026, 10, 29, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("rfc3339 = %v, %v", got, err)
	}
	if _, err := ParseOptionalDate("29/10/2026"); err == nil {
		t.Fatal("bad date accepted")
	}
}
