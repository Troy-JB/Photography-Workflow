package gallery

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBuildManifestKeepsOrderAndObjectKeys(t *testing.T) {
	galleryID := uuid.New()
	projectID := uuid.New()
	limit := 12
	g := &Gallery{
		ID:              galleryID,
		ProjectID:       projectID,
		Title:           "Wedding",
		RoundNo:         2,
		ManifestVersion: 3,
		SelectionLimit:  &limit,
	}
	now := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	rows := []manifestRow{
		{AssetID: uuid.New(), Filename: "a.jpg", Position: 1, ThumbKey: "t/thumb-a.jpg", DisplayKey: "t/display-a.jpg", LQIP: "data:image/jpeg;base64,AA", Color: "#abcdef"},
		{AssetID: uuid.New(), Filename: "b.mp4", Position: 2, ThumbKey: "t/thumb-b.jpg", DisplayKey: "t/display-b.jpg", IsVideo: true},
	}
	m := buildManifest(g, "Studio", rows, now)

	if m.ManifestVersion != 3 || m.RoundNo != 2 || m.Watermark != "Studio" {
		t.Fatalf("manifest header mismatch: %+v", m)
	}
	if len(m.Assets) != 2 {
		t.Fatalf("want 2 assets, got %d", len(m.Assets))
	}
	if m.Assets[0].Filename != "a.jpg" || m.Assets[1].Filename != "b.mp4" {
		t.Fatalf("asset order changed: %+v", m.Assets)
	}
	if !m.Assets[1].IsVideo {
		t.Fatal("video flag lost")
	}
	if m.Assets[0].LQIP == "" || m.Assets[0].Color == "" {
		t.Fatal("placeholder data lost")
	}

	// A manifest is the only thing a customer API trusts; a URL in it would
	// leak credentials and break once it expires.
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "http://") || strings.Contains(string(body), "https://") {
		t.Fatalf("manifest must not embed URLs: %s", body)
	}
	if !strings.Contains(string(body), `"thumb_key":"t/thumb-a.jpg"`) {
		t.Fatalf("object key missing: %s", body)
	}
}

func TestManifestObjectKeyIsVersioned(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	gallery := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	got := ManifestObjectKey(tenant, gallery, 4)
	want := "t/11111111-1111-1111-1111-111111111111/galleries/22222222-2222-2222-2222-222222222222/v4/manifest.json"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestValidateCreate(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	past := time.Now().Add(-time.Hour)
	zero := 0
	ok := 12

	cases := []struct {
		name string
		in   CreateInput
		err  bool
	}{
		{"trimmed title", CreateInput{Title: "  album  ", ExpiresAt: &future}, false},
		{"missing title", CreateInput{Title: "   "}, true},
		{"long title", CreateInput{Title: strings.Repeat("x", 121)}, true},
		{"past deadline", CreateInput{Title: "a", ExpiresAt: &past}, true},
		{"zero limit", CreateInput{Title: "a", SelectionLimit: &zero}, true},
		{"valid limit", CreateInput{Title: "a", SelectionLimit: &ok}, false},
		{"long watermark", CreateInput{Title: "a", WatermarkText: strings.Repeat("字", 121)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, _, _, err := validateCreate(tc.in)
			if tc.err {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("want ErrInvalidInput, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if title != strings.TrimSpace(tc.in.Title) {
				t.Fatalf("title not trimmed: %q", title)
			}
		})
	}
}

func TestValidateUpdate(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	empty := ""
	zero := 0
	if err := validateUpdate(UpdateInput{Title: &empty}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty title accepted: %v", err)
	}
	if err := validateUpdate(UpdateInput{ExpiresAt: &past}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("past deadline accepted: %v", err)
	}
	if err := validateUpdate(UpdateInput{SelectionLimit: &zero}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero limit accepted: %v", err)
	}
	if err := validateUpdate(UpdateInput{}); err != nil {
		t.Fatalf("empty patch rejected: %v", err)
	}
}

func TestWatermarkFallbacks(t *testing.T) {
	svc := &Service{cfg: Config{DefaultWatermark: "Studio Default", PublicBaseURL: "https://gallery.example.com/"}}
	if got := svc.watermarkText("  Client Album "); got != "Client Album" {
		t.Fatalf("gallery snapshot not used: %q", got)
	}
	if got := svc.watermarkText("   "); got != "Studio Default" {
		t.Fatalf("default not used: %q", got)
	}
	// The share token rides in the URL fragment so it never reaches access logs
	// or Referer headers.
	got := svc.galleryURL("abc123")
	if got != "https://gallery.example.com/#/a/abc123" {
		t.Fatalf("unexpected share url: %q", got)
	}
}

func TestDedupe(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	got := dedupe([]uuid.UUID{a, uuid.Nil, b, a})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("dedupe broken: %v", got)
	}
}

func TestDerivativesIncompleteError(t *testing.T) {
	err := &DerivativesIncompleteError{Missing: 3, Sample: []uuid.UUID{uuid.New()}}
	if !errors.Is(err, ErrDerivativesIncomplete) {
		t.Fatal("sentinel not wrapped")
	}
	if !strings.Contains(err.Error(), "3 assets") {
		t.Fatalf("unhelpful message: %v", err)
	}
}

func TestManifestRowReady(t *testing.T) {
	ready := manifestRow{ThumbKey: "a", DisplayKey: "b", ThumbComplete: true, DisplayComplete: true}
	if !ready.ready() {
		t.Fatal("complete pair rejected")
	}
	cases := []manifestRow{
		{ThumbKey: "a", DisplayKey: "b", ThumbComplete: true},
		{ThumbKey: "", DisplayKey: "b", ThumbComplete: true, DisplayComplete: true},
		{ThumbKey: "a", DisplayKey: "b", DisplayComplete: true},
	}
	for i, c := range cases {
		if c.ready() {
			t.Fatalf("case %d accepted an incomplete pair", i)
		}
	}
}

func TestValidAssetFilter(t *testing.T) {
	for _, ok := range []string{"", "all", "none", "selected", "rejected"} {
		if !ValidAssetFilter(ok) {
			t.Fatalf("filter %q should be valid", ok)
		}
	}
	for _, bad := range []string{"pending", "SELECTED", "none "} {
		if ValidAssetFilter(bad) {
			t.Fatalf("filter %q should be rejected", bad)
		}
	}
}
