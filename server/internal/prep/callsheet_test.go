package prep

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func fixedSnapshot() *CallSheetSnapshot {
	start := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	signed := time.Date(2026, 10, 30, 1, 30, 0, 0, time.UTC)
	return &CallSheetSnapshot{
		Schema:   callSheetSchema,
		Version:  1,
		FrozenAt: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC),
		Timezone: "Asia/Shanghai",
		Booking: SnapshotBooking{
			ID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
			StartsAt: start,
			EndsAt:   start.Add(4 * time.Hour),
			Note:     "Studio day",
			Resources: []SnapshotResource{
				{Kind: "studio", Name: "Studio A"},
			},
		},
		Project: SnapshotProject{
			ID:    uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			Code:  "PRJ-1",
			Title: "Autumn editorial",
		},
		Client: SnapshotClient{Name: "Acme", Company: "Acme Ltd", Phone: "13800000000"},
		Brief: &SnapshotBrief{
			Venue: "Studio A", Style: "Editorial", ShotCount: 8, Requirements: "Bring props",
		},
		ShotList: &SnapshotShotList{
			Title: "Main list",
			Items: []SnapshotShotItem{
				{
					Category: "portrait", Title: `<script>alert("x")</script>`,
					ReferenceURL: "https://example.com/ref", Priority: "high", Done: true,
				},
				{Category: "detail", Title: "Hands", Priority: "normal"},
			},
		},
		Gear: []SnapshotGear{
			{Name: "Softbox", Category: "lighting", Quantity: 2, Checked: true, Resource: "Softbox A"},
		},
		Authorizations: []SnapshotAuthorization{
			{Kind: "model_release", Subject: "Alice", Status: "signed", SignedAt: &signed},
			{Kind: "venue_permit", Subject: "Studio A", Status: "pending"},
		},
	}
}

func TestSnapshotHashIsStable(t *testing.T) {
	snap := fixedSnapshot()
	first, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("snapshot serialization is not stable")
	}
	hash := snapshotSHA256(first)
	if len(hash) != 64 || strings.ToLower(hash) != hash {
		t.Fatalf("hash = %q, want 64 lowercase hex chars", hash)
	}
	if snapshotSHA256(second) != hash {
		t.Fatalf("same snapshot produced a different hash")
	}
	if !bytes.Contains(first, []byte(`"authorizations"`)) ||
		!bytes.Contains(first, []byte(`"model_release"`)) {
		t.Fatalf("snapshot is missing authorizations: %s", first)
	}
	if bytes.Contains(first, []byte(`"authorization_note"`)) {
		t.Fatalf("snapshot leaked authorization evidence notes")
	}
}

func TestRenderCallSheetHTML(t *testing.T) {
	snap := fixedSnapshot()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	hash := snapshotSHA256(raw)
	page, err := renderCallSheetHTML(raw, hash, time.UTC)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	html := string(page)
	if strings.Contains(html, `<script>alert`) {
		t.Fatalf("raw script tag was not escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("expected escaped script text in output")
	}
	for _, want := range []string{
		"拍摄通告单", "PRJ-1", "Autumn editorial", "v1", hash,
		"Acme Ltd", "Softbox", "已核对", "肖像授权", "已签署", "待完成",
		"完成 1 / 2",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered HTML is missing %q", want)
		}
	}
	if !strings.Contains(html, "快照 SHA-256") {
		t.Fatalf("rendered HTML is missing the snapshot footer")
	}
}

func TestRenderCallSheetRejectsCorruptSnapshots(t *testing.T) {
	if _, err := renderCallSheetHTML([]byte(`{not-json`), "abc", time.UTC); !errors.Is(err, ErrSnapshotCorrupt) {
		t.Fatalf("invalid json: error = %v, want ErrSnapshotCorrupt", err)
	}
	if _, err := renderCallSheetHTML([]byte(`{"schema":"other.v9"}`), "abc", time.UTC); !errors.Is(err, ErrSnapshotCorrupt) {
		t.Fatalf("wrong schema: error = %v, want ErrSnapshotCorrupt", err)
	}
}

func TestCallSheetLabels(t *testing.T) {
	if priorityLabel("high") != "高" || priorityLabel("low") != "低" || priorityLabel("normal") != "普通" {
		t.Fatalf("unexpected priority labels")
	}
	if authorizationKindLabel("venue_permit") != "场地许可" ||
		authorizationStatusLabel("waived") != "已豁免" {
		t.Fatalf("unexpected authorization labels")
	}
}
