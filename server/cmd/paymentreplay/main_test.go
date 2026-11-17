package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// replayServer simulates the sandbox payment adapter: the first callback is
// applied, concurrent replays are answered as duplicates, and the delivery gate
// reports payment_required once the refund landed.
func replayServer(t *testing.T, alwaysProcessed bool) (*httptest.Server, *int64) {
	t.Helper()
	var calls int64
	mux := http.NewServeMux()
	mux.HandleFunc("/webhooks/payment/sandbox", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-PWF-Signature") == "" {
			t.Errorf("callback without a signature")
		}
		var ev struct {
			EventID string `json:"event_id"`
			Type    string `json:"type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("decode callback: %v", err)
		}
		if ev.EventID == "" || ev.Type != "refund.succeeded" {
			t.Errorf("unexpected event %+v", ev)
		}
		n := atomic.AddInt64(&calls, 1)
		status := "duplicate"
		if alwaysProcessed || n == 1 {
			status = "processed"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
	})
	mux.HandleFunc("/g/v1/delivery/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "payment_required"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testConfig(base string) config {
	return config{
		Base:          base,
		Provider:      "sandbox",
		Secret:        "test-secret-0123456789",
		Merchant:      "test-merchant",
		Scenario:      "refund",
		OutRefundNo:   "RF-REPLAY-1",
		RefundCents:   5000,
		Count:         5,
		Concurrent:    5,
		DeliveryToken: "link-token",
	}
}

func TestReplayProcessesExactlyOnce(t *testing.T) {
	srv, calls := replayServer(t, false)
	cfg := testConfig(srv.URL)
	rep, err := replay(context.Background(), cfg)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := atomic.LoadInt64(calls); got != int64(2*cfg.Count) {
		t.Fatalf("callbacks = %d, want %d", got, 2*cfg.Count)
	}
	if rep.Processed != 1 {
		t.Fatalf("processed = %d, want 1", rep.Processed)
	}
	if !rep.Passed {
		t.Fatalf("replay failed: %v", rep.Failures)
	}
	if rep.DeliveryGate["status"] != http.StatusConflict || rep.DeliveryGate["code"] != "payment_required" {
		t.Fatalf("delivery gate = %v", rep.DeliveryGate)
	}
}

func TestReplayRejectsDoubleSettlement(t *testing.T) {
	srv, _ := replayServer(t, true)
	rep, err := replay(context.Background(), testConfig(srv.URL))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rep.Passed {
		t.Fatal("replay passed although every callback was processed")
	}
	if len(rep.Failures) == 0 {
		t.Fatal("expected a failure explaining the double settlement")
	}
}

func TestValidateRefusesRemoteTarget(t *testing.T) {
	cfg := config{
		Base: "https://payments.example.com", Secret: "x",
		Scenario: "payment", OutTradeNo: "T1", AmountCents: 100, Count: 1, Concurrent: 1,
	}
	if err := validate(&cfg); err == nil {
		t.Fatal("validate accepted a non-loopback target without -allow-remote")
	}
	cfg.AllowRemote = true
	if err := validate(&cfg); err != nil {
		t.Fatalf("validate with -allow-remote: %v", err)
	}
}

func TestEventPayloadCarriesTheReplayedIdentity(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1")
	cfg.EventID = "evt-1"
	ev := eventPayload(cfg)
	if ev["event_id"] != "evt-1" || ev["type"] != "refund.succeeded" ||
		ev["out_refund_no"] != "RF-REPLAY-1" || ev["amount_cents"] != int64(5000) {
		t.Fatalf("payload = %+v", ev)
	}
}
