package worker

import (
	"strings"
	"testing"
	"time"
)

func TestGalleryOpenAt(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	cases := []struct {
		name     string
		status   string
		expires  *time.Time
		expected bool
	}{
		{"published without deadline", "published", nil, true},
		{"published future", "published", &future, true},
		{"published lapsed", "published", &past, false},
		{"expired", "expired", &future, false},
		{"revoked", "revoked", nil, false},
		{"draft", "draft", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := galleryOpenAt(c.status, c.expires, now); got != c.expected {
				t.Fatalf("open = %v, want %v", got, c.expected)
			}
		})
	}
}

func TestDeadlineBodyUsesScheduleTimezone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load timezone: %v", err)
	}
	expires := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC) // 18:00 in Shanghai
	body := deadlineBody("秋日写真", expires, shanghai)
	if !strings.Contains(body, "《秋日写真》") || !strings.Contains(body, "2026-11-05 18:00") {
		t.Fatalf("body = %q", body)
	}
}
