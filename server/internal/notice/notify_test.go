package notice

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/webhook"
)

func TestApplicableMatrix(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	start := now.Add(time.Hour)
	hold := now.Add(10 * time.Minute)
	cases := []struct {
		name    string
		subject string
		typ     string
		state   DeliveryState
		want    bool
	}{
		{"shoot confirmed future", "booking", TypeShootReminder, DeliveryState{Found: true, BookingStatus: "confirmed", StartsAt: start}, true},
		{"shoot started", "booking", TypeShootReminder, DeliveryState{Found: true, BookingStatus: "confirmed", StartsAt: now.Add(-time.Minute)}, false},
		{"shoot cancelled", "booking", TypeShootReminder, DeliveryState{Found: true, BookingStatus: "cancelled", StartsAt: start}, false},
		{"shoot missing", "booking", TypeShootReminder, DeliveryState{}, false},
		{"hold held future", "booking", TypeHoldExpiry, DeliveryState{Found: true, BookingStatus: "held", HoldExpiresAt: &hold}, true},
		{"hold confirmed", "booking", TypeHoldExpiry, DeliveryState{Found: true, BookingStatus: "confirmed", HoldExpiresAt: &hold}, false},
		{"hold expired", "booking", TypeHoldExpiry, DeliveryState{Found: true, BookingStatus: "held", HoldExpiresAt: timePtr(now.Add(-time.Second))}, false},
		{"auth pending", "booking", TypeAuthIncomplete, DeliveryState{Found: true, BookingStatus: "confirmed", StartsAt: start, PendingAuths: 2}, true},
		{"auth cleared", "booking", TypeAuthIncomplete, DeliveryState{Found: true, BookingStatus: "confirmed", StartsAt: start}, false},
		// Staff-side gallery events have no booking state to re-check; the pending
		// row is deliverable on its own.
		{"gallery submitted", "gallery", "gallery_submitted", DeliveryState{}, true},
		{"gallery deadline", "gallery", "gallery_deadline", DeliveryState{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &Notification{Type: c.typ, SubjectType: c.subject}
			if got := Applicable(n, &c.state, now); got != c.want {
				t.Fatalf("applicable = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDeliverSkipsWithoutWebhookOrWhenStale(t *testing.T) {
	svc := &Service{cfg: Config{Enabled: true, Timezone: time.UTC}}
	n := &Notification{ID: uuid.New(), Type: TypeShootReminder, SubjectType: "booking"}
	state := &DeliveryState{Found: true, BookingStatus: "confirmed", StartsAt: time.Now().Add(time.Hour)}
	if status, err := svc.Deliver(context.Background(), http.DefaultClient, n, state, time.Now()); err != nil || status != StatusSkipped {
		t.Fatalf("no webhook = %q/%v", status, err)
	}
	svc.cfg.WebhookURL = "http://127.0.0.1:1/hook"
	if status, err := svc.Deliver(context.Background(), http.DefaultClient, n,
		&DeliveryState{Found: true, BookingStatus: "cancelled"}, time.Now()); err != nil || status != StatusSkipped {
		t.Fatalf("stale reminder = %q/%v", status, err)
	}
}

func TestDeliverSignsAndPostsPayload(t *testing.T) {
	const secret = "0123456789abcdef"
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	n := &Notification{
		ID: uuid.New(), TenantID: uuid.New(), Type: TypeShootReminder,
		SubjectType: "booking", SubjectID: uuid.New(), Title: "拍摄提醒",
		Body: "明天见", Payload: json.RawMessage(`{"booking_id":"x"}`),
		DueAt: now, CreatedAt: now,
	}
	svc := &Service{cfg: Config{
		Enabled: true, WebhookURL: srv.URL, WebhookSecret: secret, WebhookTimeout: time.Second,
		Timezone: time.UTC,
	}}
	state := &DeliveryState{Found: true, BookingStatus: "confirmed", StartsAt: now.Add(time.Hour)}
	status, err := svc.Deliver(context.Background(), srv.Client(), n, state, now)
	if err != nil || status != StatusSent {
		t.Fatalf("deliver = %q/%v", status, err)
	}
	if got.Header.Get("X-PWF-Event") != "notification" ||
		got.Header.Get("X-PWF-Notification-Type") != TypeShootReminder {
		t.Fatalf("headers = %v", got.Header)
	}
	if got.Header.Get("Idempotency-Key") != n.ID.String() {
		t.Fatalf("idempotency key = %q, want notification id %s",
			got.Header.Get("Idempotency-Key"), n.ID)
	}
	if err := webhook.VerifySignature(secret, got.Header.Get("X-PWF-Webhook-Signature"), body, time.Now(), time.Minute); err != nil {
		t.Fatalf("signature: %v", err)
	}
	var payload WebhookNotification
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.Event != "notification" || payload.NotificationID != n.ID ||
		payload.SubjectID != n.SubjectID || payload.Title != n.Title {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestDeliverClassifiesPermanentAndRetryableFailures(t *testing.T) {
	now := time.Now().UTC()
	n := &Notification{ID: uuid.New(), Type: TypeShootReminder, SubjectType: "booking", SubjectID: uuid.New()}
	state := &DeliveryState{Found: true, BookingStatus: "confirmed", StartsAt: now.Add(time.Hour)}
	for _, tc := range []struct {
		code      int
		permanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusInternalServerError, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
		}))
		svc := &Service{cfg: Config{Enabled: true, WebhookURL: srv.URL, WebhookSecret: "0123456789abcdef"}}
		_, err := svc.Deliver(context.Background(), srv.Client(), n, state, now)
		if err == nil {
			t.Fatalf("status %d returned nil error", tc.code)
		}
		if errors.Is(err, webhook.ErrPermanentDelivery) != tc.permanent {
			t.Fatalf("status %d permanent = %v: %v", tc.code, !tc.permanent, err)
		}
		srv.Close()
	}
}

func timePtr(t time.Time) *time.Time { return &t }
