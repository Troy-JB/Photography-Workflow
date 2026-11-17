package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCheckAccessGateOrder(t *testing.T) {
	cases := []struct {
		name    string
		state   accessState
		paid    bool
		ready   bool
		wantErr error
	}{
		{
			name: "archived project wins even when paid",
			state: accessState{ProjectState: "archived", PaymentStatus: "paid",
				RoundStatus: "approved", Delivery: Delivery{Status: StatusReady}},
			paid: true, ready: true, wantErr: ErrProjectArchived,
		},
		{
			name: "cancelled project",
			state: accessState{ProjectState: "cancelled", PaymentStatus: "paid",
				RoundStatus: "approved", Delivery: Delivery{Status: StatusReady}},
			paid: true, ready: true, wantErr: ErrProjectCancelled,
		},
		{
			name: "unpaid blocks before round check",
			state: accessState{ProjectState: "completed", PaymentStatus: "balance_pending",
				RoundStatus: "resubmitted", Delivery: Delivery{Status: StatusBuilding}},
			paid: true, wantErr: ErrPaymentRequired,
		},
		{
			name: "round must stay approved",
			state: accessState{ProjectState: "completed", PaymentStatus: "paid",
				RoundStatus: "retouching", Delivery: Delivery{Status: StatusBuilding}},
			paid: true, wantErr: ErrRoundNotApproved,
		},
		{
			name: "download requires ready delivery",
			state: accessState{ProjectState: "completed", PaymentStatus: "paid",
				RoundStatus: "approved", Delivery: Delivery{Status: StatusBuilding}},
			paid: true, ready: true, wantErr: ErrDeliveryNotReady,
		},
		{
			name: "build only requires payment",
			state: accessState{ProjectState: "completed", PaymentStatus: "paid",
				RoundStatus: "approved", Delivery: Delivery{Status: StatusPending}},
			paid: true, wantErr: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkAccess(&tc.state, tc.paid, tc.ready)
			if got != tc.wantErr {
				t.Fatalf("got %v want %v", got, tc.wantErr)
			}
		})
	}
}

func TestBuildEntriesWebPackageSkipsUnsupported(t *testing.T) {
	s := &Service{}
	revisionID := uuid.New()
	items := []buildItem{
		{AssetID: uuid.New(), Position: 1, OriginalName: "portrait.jpg",
			WebStatus: WebReady, WebKey: "t/x/v1/web/1.jpg", WebSize: 100, WebSHA256: "a",
			WebWidth: 2048, WebHeight: 1365},
		{AssetID: uuid.New(), Position: 2, OriginalName: "raw.CR3",
			WebStatus: WebSkipped, WebWarningCode: WarningUnsupported},
		{AssetID: uuid.New(), Position: 3, OriginalName: "retouched.jpg", RevisionID: &revisionID,
			RevisionNo: 2, OriginalKey: "t/x/revisions/a/v2/source.jpg", OriginalSize: 500,
			OriginalSHA256: "b", WebStatus: WebReady, WebKey: "t/x/v1/web/3.jpg",
			WebSize: 90, WebSHA256: "c", WebWidth: 1600, WebHeight: 1200},
	}

	entries, metas, warnings := s.buildEntries("WED-01", SpecWeb, items)
	if len(entries) != 2 || len(metas) != 2 {
		t.Fatalf("web package should contain the two rendered items, got %d entries %d metas", len(entries), len(metas))
	}
	if entries[0].Name != "WED-01_0001_portrait.jpg" {
		t.Fatalf("entry name: %s", entries[0].Name)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], WarningUnsupported) {
		t.Fatalf("skipped item must be reported: %v", warnings)
	}
	if metas[1].Source != "revision" || metas[1].RevisionNo != 2 {
		t.Fatalf("revision source lost: %+v", metas[1])
	}
	if entries[0].ExpectedSHA256 != "a" || entries[0].ExpectedSize != 100 {
		t.Fatalf("web integrity snapshot lost: %+v", entries[0])
	}

	entries, metas, warnings = s.buildEntries("WED-01", SpecOriginal, items)
	if len(entries) != 3 || len(metas) != 3 {
		t.Fatalf("original package must contain every item, got %d entries", len(entries))
	}
	if entries[1].Name != "WED-01_0002_raw.cr3" {
		t.Fatalf("original keeps its extension: %s", entries[1].Name)
	}
	if entries[2].ExpectedSHA256 != "b" || entries[2].ExpectedSize != 500 {
		t.Fatalf("revision bytes must be the original payload: %+v", entries[2])
	}
	if metas[1].Warning != WarningUnsupported {
		t.Fatalf("warning must be mirrored in the original manifest: %+v", metas[1])
	}
	if len(warnings) != 1 {
		t.Fatalf("original package reports skipped web renders once: %v", warnings)
	}
}

func TestBuildEntriesWebManifestOnly(t *testing.T) {
	s := &Service{}
	items := []buildItem{
		{AssetID: uuid.New(), Position: 1, OriginalName: "a.CR3",
			WebStatus: WebSkipped, WebWarningCode: WarningUnsupported},
	}
	entries, metas, warnings := s.buildEntries("WED", SpecWeb, items)
	if len(entries) != 0 || len(metas) != 0 {
		t.Fatalf("no renderable item must produce an empty web package")
	}
	if len(warnings) != 1 {
		t.Fatalf("warning missing: %v", warnings)
	}
}

func TestKeyVersion(t *testing.T) {
	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	cases := map[string]int{
		"t/11111111-1111-1111-1111-111111111111/deliveries/" + id.String() + "/v3/web/1.jpg":     3,
		"t/11111111-1111-1111-1111-111111111111/deliveries/" + id.String() + "/v12/original.zip": 12,
		"t/other/deliveries/" + id.String() + "/v2/web/1.jpg":                                    2,
		"t/11111111-1111-1111-1111-111111111111/galleries/" + id.String() + "/v4/manifest.json":  0,
		"t/11111111-1111-1111-1111-111111111111/deliveries/" + id.String() + "/web/1.jpg":        0,
	}
	for key, want := range cases {
		if got := keyVersion(key, id); got != want {
			t.Fatalf("keyVersion(%q)=%d want %d", key, got, want)
		}
	}
}

func TestRevisionMime(t *testing.T) {
	if got := revisionMime("t/x/revisions/a/v1/source.PNG"); got != "image/png" {
		t.Fatalf("png: %s", got)
	}
	if got := revisionMime("t/x/revisions/a/v1/source.webp"); got != "image/webp" {
		t.Fatalf("webp: %s", got)
	}
	if got := revisionMime("t/x/revisions/a/v1/source.jpg"); got != "image/jpeg" {
		t.Fatalf("jpg: %s", got)
	}
}

func TestCleanupTempOnlyRemovesOldSpools(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, tempPrefix+"old")
	fresh := filepath.Join(dir, tempPrefix+"fresh")
	foreign := filepath.Join(dir, "unrelated")
	for _, name := range []string{old, fresh, foreign} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	s := NewService(nil, nil, nil, Config{TempDir: dir}, nil)
	removed, err := s.CleanupTemp(now)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d files, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale spool survived")
	}
	for _, name := range []string{fresh, foreign} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("%s must be kept: %v", name, err)
		}
	}
}

func TestTruncateTextKeepsRunes(t *testing.T) {
	got := truncateText("交付构建失败", 2)
	if got != "交付" {
		t.Fatalf("runes split: %q", got)
	}
	if truncateText("abc", 0) != "" {
		t.Fatal("zero limit must return empty")
	}
}

func TestWebFileName(t *testing.T) {
	if got := WebFileName("DSC_0001.CR3"); got != "DSC_0001.jpg" {
		t.Fatalf("got %s", got)
	}
	if got := WebFileName("../../etc/passwd"); got != "passwd.jpg" {
		t.Fatalf("path traversal not stripped: %s", got)
	}
	if got := WebFileName(""); got != "file.jpg" {
		t.Fatalf("empty name: %s", got)
	}
}
