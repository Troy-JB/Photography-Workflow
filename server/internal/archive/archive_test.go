package archive

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestClassifyItem(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name         string
		hashErr      error
		expectedSHA  string
		expectedSize int64
		actualSHA    string
		actualSize   int64
		wantStatus   string
		wantNote     string
	}{
		{
			name:        "matching hash and size verifies",
			expectedSHA: sha, expectedSize: 1024, actualSHA: sha, actualSize: 1024,
			wantStatus: ItemVerified,
		},
		{
			name:        "missing object wins over recorded metadata",
			hashErr:     errObjectMissing,
			expectedSHA: sha, expectedSize: 1024,
			wantStatus: ItemMissing, wantNote: "对象不存在",
		},
		{
			name:        "hash mismatch",
			expectedSHA: sha, expectedSize: 1024,
			actualSHA: strings.Repeat("f", 64), actualSize: 1024,
			wantStatus: ItemMismatch, wantNote: "SHA-256 与快照不一致",
		},
		{
			name:        "size mismatch",
			expectedSHA: sha, expectedSize: 1024, actualSHA: sha, actualSize: 1023,
			wantStatus: ItemMismatch, wantNote: "对象大小与快照不一致",
		},
		{
			name:         "size is still checked without a recorded hash",
			expectedSize: 512, actualSize: 511,
			wantStatus: ItemMismatch, wantNote: "对象大小与快照不一致",
		},
		{
			name:       "existence-only verification is recorded in the note",
			actualSize: 512,
			wantStatus: ItemVerified, wantNote: "未记录哈希，仅校验对象存在性",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, note := classifyItem(tc.hashErr, tc.expectedSHA, tc.expectedSize,
				tc.actualSHA, tc.actualSize)
			if status != tc.wantStatus || note != tc.wantNote {
				t.Fatalf("got (%q, %q) want (%q, %q)", status, note, tc.wantStatus, tc.wantNote)
			}
		})
	}
}

func TestOwnedObjectKey(t *testing.T) {
	svc := &Service{}
	tenant := uuid.New()
	other := uuid.New()
	cases := []struct {
		key  string
		want bool
	}{
		{"t/" + tenant.String() + "/assets/a.jpg", true},
		{"tenants/" + tenant.String() + "/contracts/c.pdf", true},
		{"t/" + other.String() + "/assets/a.jpg", false},
		{"tenants/" + tenant.String() + "x/assets/a.jpg", false},
		{"public/logo.png", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := svc.ownedObjectKey(tenant, tc.key); got != tc.want {
			t.Fatalf("ownedObjectKey(%q) = %v want %v", tc.key, got, tc.want)
		}
	}
}

func TestRequestDeletionRequiresExplicitConfirmation(t *testing.T) {
	svc := &Service{}
	tenant, actor, id := uuid.New(), uuid.New(), uuid.New()
	cases := []struct {
		name    string
		confirm string
		reason  string
	}{
		{"missing confirmation", "", "customer asked"},
		{"wrong confirmation word", "delete", "customer asked"},
		{"confirmation with extra words", "DELETE NOW", "customer asked"},
		{"missing reason", "DELETE", "  "},
		{"reason too long", "DELETE", strings.Repeat("删", 501)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.RequestDeletion(t.Context(), tenant, actor, "", id, tc.confirm, tc.reason)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("got %v want ErrInvalidInput", err)
			}
		})
	}
}

func TestSetPolicyValidation(t *testing.T) {
	svc := &Service{}
	tenant, actor := uuid.New(), uuid.New()
	cases := []struct {
		name      string
		keep      int
		warn      int
		wantInput bool
	}{
		{"keep below range", 0, 0, true},
		{"keep above range", 3651, 0, true},
		{"negative warning", 365, -1, true},
		{"warning equal to keep", 365, 365, true},
		{"warning above keep", 30, 31, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SetPolicy(t.Context(), tenant, actor, "", tc.keep, tc.warn)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("got %v want ErrInvalidInput", err)
			}
		})
	}
}

func TestNewServiceNormalizesConfig(t *testing.T) {
	svc := NewService(nil, nil, Config{}, nil)
	if svc.SweepInterval() != 5*time.Minute {
		t.Fatalf("sweep interval = %s want 5m", svc.SweepInterval())
	}
	if svc.cfg.DefaultKeepDays != 365 || svc.cfg.DefaultWarnDays != 30 {
		t.Fatalf("defaults = %d/%d want 365/30", svc.cfg.DefaultKeepDays, svc.cfg.DefaultWarnDays)
	}
	if svc.cfg.Timezone != time.UTC {
		t.Fatalf("timezone = %v want UTC", svc.cfg.Timezone)
	}

	tight := NewService(nil, nil, Config{DefaultKeepDays: 10, DefaultWarnDays: 30}, nil)
	if tight.cfg.DefaultWarnDays >= tight.cfg.DefaultKeepDays {
		t.Fatalf("warn (%d) must be below keep (%d)", tight.cfg.DefaultWarnDays, tight.cfg.DefaultKeepDays)
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		1023:    "1023 B",
		1024:    "1.0 KB",
		1536:    "1.5 KB",
		5 << 20: "5.0 MB",
		3 << 30: "3.0 GB",
		2 << 40: "2.0 TB",
	}
	for in, want := range cases {
		if got := formatBytes(in); got != want {
			t.Fatalf("formatBytes(%d) = %q want %q", in, got, want)
		}
	}
}
