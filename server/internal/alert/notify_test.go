package alert

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSignatureRoundTrip(t *testing.T) {
	secret := "test-alert-secret-0123456789"
	body := []byte(`{"event":"payment_alert"}`)
	at := time.Unix(1_700_000_000, 0)

	header := Signature(secret, at, body)
	if err := VerifySignature(secret, header, body, at, 5*time.Minute); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := VerifySignature(secret, header, append(body, ' '), at, 5*time.Minute); err == nil {
		t.Fatal("tampered body verified")
	}
	if err := VerifySignature(secret+"x", header, body, at, 5*time.Minute); err == nil {
		t.Fatal("wrong secret verified")
	}
	if err := VerifySignature(secret, header, body, at.Add(10*time.Minute), 5*time.Minute); err == nil {
		t.Fatal("stale timestamp verified")
	}
	if err := VerifySignature(secret, "", body, at, 5*time.Minute); err == nil {
		t.Fatal("missing header verified")
	}
}

func TestSendWebhookClassification(t *testing.T) {
	var gotSignature, gotEvent, gotTimestamp, gotIdempotencyKey string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature = r.Header.Get("X-PWF-Webhook-Signature")
		gotEvent = r.Header.Get("X-PWF-Event")
		gotTimestamp = r.Header.Get("X-PWF-Webhook-Timestamp")
		gotIdempotencyKey = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	cfg := Config{WebhookURL: ok.URL, WebhookSecret: "test-alert-secret-0123456789", WebhookTimeout: time.Second}
	n := Notification{
		Event: "payment_alert", AlertID: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Code: "late_payment", Severity: SeverityCritical, Status: StatusOpen,
	}
	if err := SendWebhook(context.Background(), ok.Client(), cfg, n); err != nil {
		t.Fatalf("success delivery: %v", err)
	}
	if gotEvent != "payment_alert" || gotTimestamp == "" || gotSignature == "" {
		t.Fatalf("headers = event %q ts %q sig %q", gotEvent, gotTimestamp, gotSignature)
	}
	if gotIdempotencyKey != n.AlertID.String() {
		t.Fatalf("idempotency key = %q, want alert id %s", gotIdempotencyKey, n.AlertID)
	}

	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer reject.Close()
	cfg.WebhookURL = reject.URL
	if err := SendWebhook(context.Background(), reject.Client(), cfg, n); err == nil {
		t.Fatal("4xx webhook did not fail")
	} else if !errors.Is(err, ErrPermanentDelivery) {
		t.Fatalf("4xx should be permanent, got %v", err)
	}

	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer unavailable.Close()
	cfg.WebhookURL = unavailable.URL
	if err := SendWebhook(context.Background(), unavailable.Client(), cfg, n); err == nil {
		t.Fatal("5xx webhook did not fail")
	} else if errors.Is(err, ErrPermanentDelivery) {
		t.Fatalf("5xx must be retryable, got %v", err)
	}
}

func TestValidateInput(t *testing.T) {
	good := Input{Code: "late_payment", Severity: SeverityCritical, DedupeKey: "late:1", Title: "t"}
	if err := validateInput(good); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	cases := []Input{
		{Code: "", Severity: SeverityInfo, DedupeKey: "k", Title: "t"},
		{Code: "c", Severity: "fatal", DedupeKey: "k", Title: "t"},
		{Code: "c", Severity: SeverityInfo, DedupeKey: "", Title: "t"},
		{Code: "c", Severity: SeverityInfo, DedupeKey: "k", Title: ""},
	}
	for i, in := range cases {
		if err := validateInput(in); err == nil {
			t.Fatalf("case %d accepted invalid input %+v", i, in)
		}
	}
}

func TestNotificationPayloadEncodesDetail(t *testing.T) {
	n := NewNotification(&Alert{
		ID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TenantID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Code:     "callback_amount_mismatch",
		Severity: SeverityCritical,
		Status:   StatusOpen,
		Title:    "t",
		Detail:   json.RawMessage(`{"reason":"amount_mismatch"}`),
	})
	raw, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round Notification
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.Event != "payment_alert" || round.Code != "callback_amount_mismatch" {
		t.Fatalf("round trip = %+v", round)
	}
}
