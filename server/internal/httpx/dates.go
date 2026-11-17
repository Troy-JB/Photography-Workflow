package httpx

import (
	"fmt"
	"strings"
	"time"
)

// ParseOptionalDate accepts an empty string, a date (2006-01-02) or RFC3339.
func ParseOptionalDate(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("date must be YYYY-MM-DD or RFC3339: %q", raw)
}
