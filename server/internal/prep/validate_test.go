package prep

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWritableBookingStatus(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{
		{"held", true},
		{"confirmed", true},
		{"cancelled", false},
		{"expired", false},
		{"", false},
	} {
		if got := writableBookingStatus(tc.status); got != tc.want {
			t.Fatalf("writableBookingStatus(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestValidateReferenceURL(t *testing.T) {
	for _, ok := range []string{"", "http://example.com/a.jpg", "https://example.com/board"} {
		if err := validateReferenceURL(ok); err != nil {
			t.Fatalf("validateReferenceURL(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{
		"javascript:alert(1)",
		"ftp://example.com/a.jpg",
		"//example.com/a.jpg",
		"https:///missing-host",
		"https://example.com/" + strings.Repeat("a", 1000),
	} {
		if err := validateReferenceURL(bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("validateReferenceURL(%q) = %v, want ErrInvalidInput", bad, err)
		}
	}
}

func TestValidateShotItem(t *testing.T) {
	if err := validateShotItem("portrait", "Hero", "desc", "https://example.com/ref", "high"); err != nil {
		t.Fatalf("valid item rejected: %v", err)
	}
	cases := []struct {
		name     string
		category string
		title    string
		desc     string
		url      string
		priority string
	}{
		{"missing title", "portrait", "  ", "", "", "normal"},
		{"long title", "portrait", strings.Repeat("a", 201), "", "", "normal"},
		{"long category", strings.Repeat("a", 121), "Hero", "", "", "normal"},
		{"long description", "portrait", "Hero", strings.Repeat("a", 4001), "", "normal"},
		{"bad url", "portrait", "Hero", "", "javascript:alert(1)", "normal"},
		{"bad priority", "portrait", "Hero", "", "", "urgent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateShotItem(c.category, c.title, c.desc, c.url, c.priority); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestPrepareShotItemInputDefaultsPriority(t *testing.T) {
	in, err := prepareShotItemInput(ShotItemInput{Title: "Hero"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if in.Priority != "normal" {
		t.Fatalf("priority = %q, want normal", in.Priority)
	}
}

func TestValidateGear(t *testing.T) {
	if err := validateGear("Softbox", "lighting", "", 2); err != nil {
		t.Fatalf("valid gear rejected: %v", err)
	}
	for _, tc := range []struct {
		name     string
		quantity int
	}{
		{"zero", 0},
		{"too many", 101},
		{"negative", -1},
	} {
		if err := validateGear("Softbox", "", "", tc.quantity); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: error = %v, want ErrInvalidInput", tc.name, err)
		}
	}
	if err := validateGear(" ", "", "", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("blank name: error = %v, want ErrInvalidInput", err)
	}
}

func TestNextAuthorizationState(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	next, err := nextAuthorizationState(authorizationState{Status: "pending"}, "signed", "", now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if next.Status != "signed" || next.SignedAt == nil || !next.SignedAt.Equal(now) {
		t.Fatalf("signed state = %+v, want server timestamp %v", next, now)
	}

	prior := now.Add(-48 * time.Hour)
	next, err = nextAuthorizationState(
		authorizationState{Status: "signed", SignedAt: &prior}, "signed", "updated", now)
	if err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	if next.SignedAt == nil || !next.SignedAt.Equal(prior) {
		t.Fatalf("re-sign changed signed_at to %v, want %v", next.SignedAt, prior)
	}

	next, err = nextAuthorizationState(
		authorizationState{Status: "signed", SignedAt: &prior}, "waived", "client cancelled", now)
	if err != nil {
		t.Fatalf("waive: %v", err)
	}
	if next.SignedAt != nil {
		t.Fatalf("waived state kept signed_at %v", next.SignedAt)
	}

	if _, err := nextAuthorizationState(authorizationState{Status: "pending"}, "waived", "  ", now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("waived without note: error = %v, want ErrInvalidInput", err)
	}
	if _, err := nextAuthorizationState(authorizationState{Status: "pending"}, "unknown", "", now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown status: error = %v, want ErrInvalidInput", err)
	}
	if _, err := nextAuthorizationState(authorizationState{Status: "pending"}, "pending", strings.Repeat("a", 2001), now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("long note: error = %v, want ErrInvalidInput", err)
	}
}

func TestPrepareAuthorizationInput(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	in, state, err := prepareAuthorizationInput(AuthorizationInput{Kind: "model_release", Subject: " Alice "}, now)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if in.Subject != "Alice" || state.Status != "pending" || state.SignedAt != nil {
		t.Fatalf("prepared = %+v / %+v", in, state)
	}
	if _, _, err := prepareAuthorizationInput(AuthorizationInput{Kind: "selfie"}, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown kind: error = %v, want ErrInvalidInput", err)
	}
	_, state, err = prepareAuthorizationInput(AuthorizationInput{Kind: "venue_permit", Status: "signed"}, now)
	if err != nil {
		t.Fatalf("prepare signed: %v", err)
	}
	if state.Status != "signed" || state.SignedAt == nil || !state.SignedAt.Equal(now) {
		t.Fatalf("signed state = %+v", state)
	}
}
