package notice

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testConfig() Config {
	return Config{
		Enabled:    true,
		ShootLeads: []time.Duration{24 * time.Hour, 2 * time.Hour},
		AuthLead:   24 * time.Hour,
		HoldLead:   10 * time.Minute,
		Timezone:   time.FixedZone("CST", 8*3600),
	}
}

func TestPlanForConfirmedBooking(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	start := now.Add(30 * time.Hour)
	state := bookingState{ID: uuid.New(), ProjectID: uuid.New(), Status: "confirmed", StartsAt: start, PendingAuths: 1}
	got := planFor(now, state, testConfig())
	if len(got) != 3 {
		t.Fatalf("planned %d reminders, want 3: %+v", len(got), got)
	}
	byType := map[string]plannedNotice{}
	for _, n := range got {
		byType[n.Type] = n
	}
	if n := byType[TypeShootReminder]; len(n.DedupeKey) == 0 {
		t.Fatal("missing shoot reminder")
	}
	// The 24h lead is the first shoot reminder in descending order.
	first := got[0]
	if first.Type != TypeShootReminder || !first.DueAt.Equal(start.Add(-24*time.Hour)) {
		t.Fatalf("first reminder = %+v", first)
	}
	second := got[1]
	if second.Type != TypeShootReminder || !second.DueAt.Equal(start.Add(-2*time.Hour)) {
		t.Fatalf("second reminder = %+v", second)
	}
	auth := byType[TypeAuthIncomplete]
	if !auth.DueAt.Equal(start.Add(-24*time.Hour)) || !strings.Contains(auth.DedupeKey, fmt.Sprint(start.Unix())) {
		t.Fatalf("auth reminder = %+v", auth)
	}
	if strings.Contains(auth.Body, "\n") {
		t.Fatalf("body should be single-line: %q", auth.Body)
	}
}

func TestPlanForDedupeKeysFollowStartsAt(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	id := uuid.New()
	cfg := testConfig()
	a := planFor(now, bookingState{ID: id, Status: "confirmed", StartsAt: now.Add(48 * time.Hour)}, cfg)
	b := planFor(now, bookingState{ID: id, Status: "confirmed", StartsAt: now.Add(50 * time.Hour)}, cfg)
	if a[0].DedupeKey == b[0].DedupeKey {
		t.Fatalf("dedupe key did not include starts_at: %q", a[0].DedupeKey)
	}
	if !strings.Contains(a[0].DedupeKey, "shoot:"+id.String()+":") {
		t.Fatalf("unexpected key %q", a[0].DedupeKey)
	}
}

func TestPlanForImmediateAndSkippedWindows(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cfg := testConfig()
	soon := planFor(now, bookingState{ID: uuid.New(), Status: "confirmed", StartsAt: now.Add(time.Hour)}, cfg)
	for _, n := range soon {
		if !n.DueAt.Equal(now) {
			t.Fatalf("past-due reminder = %+v, want immediate", n)
		}
	}
	if got := planFor(now, bookingState{ID: uuid.New(), Status: "confirmed", StartsAt: now.Add(-time.Minute)}, cfg); len(got) != 0 {
		t.Fatalf("started shoot planned reminders: %+v", got)
	}
	if got := planFor(now, bookingState{ID: uuid.New(), Status: "cancelled", StartsAt: now.Add(time.Hour)}, cfg); len(got) != 0 {
		t.Fatalf("cancelled booking planned reminders: %+v", got)
	}
}

func TestPlanForDisabledLeads(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.AuthLead = 0
	cfg.HoldLead = 0
	cfg.Enabled = false
	if got := planFor(now, bookingState{ID: uuid.New(), Status: "confirmed", StartsAt: now.Add(time.Hour), PendingAuths: 1}, cfg); len(got) != 0 {
		t.Fatalf("disabled planner returned %+v", got)
	}
	cfg.Enabled = true
	got := planFor(now, bookingState{ID: uuid.New(), Status: "confirmed", StartsAt: now.Add(time.Hour), PendingAuths: 1}, cfg)
	for _, n := range got {
		if n.Type == TypeAuthIncomplete {
			t.Fatalf("zero auth lead still planned %+v", n)
		}
	}
	held := planFor(now, bookingState{ID: uuid.New(), Status: "held", HoldExpiresAt: ptr(now.Add(time.Hour))}, cfg)
	if len(held) != 0 {
		t.Fatalf("zero hold lead still planned %+v", held)
	}
}

func TestPlanForHoldExpiry(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	expires := now.Add(30 * time.Minute)
	got := planFor(now, bookingState{ID: uuid.New(), Status: "held", HoldExpiresAt: &expires}, testConfig())
	if len(got) != 1 || got[0].Type != TypeHoldExpiry {
		t.Fatalf("hold plan = %+v", got)
	}
	if !got[0].DueAt.Equal(expires.Add(-10 * time.Minute)) {
		t.Fatalf("hold due = %v", got[0].DueAt)
	}
	if expired := planFor(now, bookingState{ID: uuid.New(), Status: "held", HoldExpiresAt: ptr(now.Add(-time.Minute))}, testConfig()); len(expired) != 0 {
		t.Fatalf("expired hold planned %+v", expired)
	}
}

func ptr(t time.Time) *time.Time { return &t }
