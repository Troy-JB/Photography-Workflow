package payment

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testSecret   = "test-payment-secret-0123456789"
	testMerchant = "merchant-001"
	testPayBase  = "http://localhost:8080/pay"
)

func newTestSandbox(t *testing.T) *Sandbox {
	t.Helper()
	sb, err := NewSandbox(testSecret, testMerchant, testPayBase, 5*time.Minute)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	return sb
}

func TestNewSandboxValidation(t *testing.T) {
	if _, err := NewSandbox("short", testMerchant, testPayBase, time.Minute); err == nil {
		t.Fatal("expected short secret to be rejected")
	}
	if _, err := NewSandbox(testSecret, "", testPayBase, time.Minute); err == nil {
		t.Fatal("expected empty merchant id to be rejected")
	}
	if _, err := NewSandbox(testSecret, testMerchant, "", time.Minute); err == nil {
		t.Fatal("expected empty pay base url to be rejected")
	}
}

func TestCreateOrderAndRefund(t *testing.T) {
	sb := newTestSandbox(t)
	order, err := sb.CreateOrder(t.Context(), OrderRequest{
		OutTradeNo: "PWFABC", Currency: "CNY", AmountCents: 50000,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if !strings.HasPrefix(order.ProviderOrderID, "sbxo_") || !strings.Contains(order.PayURL, "PWFABC") {
		t.Fatalf("unexpected order params: %+v", order)
	}

	refund, err := sb.Refund(t.Context(), RefundRequest{
		OutTradeNo: "PWFABC", OutRefundNo: "RFDXYZ", Reason: "test", Currency: "CNY", AmountCents: 50000,
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if refund.Status != RefundPending || !strings.HasPrefix(refund.ProviderRefundID, "sbxr_") {
		t.Fatalf("unexpected refund result: %+v", refund)
	}
	if _, err := sb.Refund(t.Context(), RefundRequest{OutRefundNo: "RFDXYZ", AmountCents: 0}); err == nil {
		t.Fatal("expected non-positive refund to be rejected")
	}
}

func TestCompleteOrderCreatesNormalPaymentEvent(t *testing.T) {
	sb := newTestSandbox(t)
	if _, err := sb.CreateOrder(t.Context(), OrderRequest{OutTradeNo: "PWF-COMPLETE", Currency: "CNY", AmountCents: 12000}); err != nil {
		t.Fatal(err)
	}
	ev, err := sb.CompleteOrder("PWF-COMPLETE")
	if err != nil {
		t.Fatalf("CompleteOrder: %v", err)
	}
	if ev.Type != EventPaymentSucceeded || ev.OutTradeNo != "PWF-COMPLETE" || ev.AmountCents != 12000 || ev.Currency != "CNY" || len(ev.Raw) == 0 {
		t.Fatalf("unexpected sandbox event: %+v", ev)
	}
	state, err := sb.QueryOrder(t.Context(), "PWF-COMPLETE")
	if err != nil || state.Status != OrderPaid {
		t.Fatalf("sandbox order state = %+v, err = %v", state, err)
	}
	if _, err := sb.CompleteOrder("PWF-COMPLETE"); !errors.Is(err, ErrOrderNotPending) {
		t.Fatalf("second completion err = %v, want pending-state rejection", err)
	}
}

func signBody(secret string, at time.Time, body []byte) string {
	return SignRequestBody(secret, at, body)
}

func callbackRequest(t *testing.T, secret string, at time.Time, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/payment/sandbox", bytes.NewReader(body))
	req.Header.Set("X-PWF-Signature", signBody(secret, at, body))
	return req
}

func paymentEventBody(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	ev := map[string]any{
		"event_id":          "evt-001",
		"type":              EventPaymentSucceeded,
		"merchant_id":       testMerchant,
		"out_trade_no":      "PWFABC",
		"provider_trade_no": "wx-001",
		"amount_cents":      50000,
		"currency":          "CNY",
		"paid_at":           time.Now().UTC().Format(time.RFC3339),
	}
	if mutate != nil {
		mutate(ev)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return raw
}

func TestQueryAndCloseOrder(t *testing.T) {
	sb := newTestSandbox(t)
	if _, err := sb.QueryOrder(t.Context(), "PWFMISSING"); !errors.Is(err, ErrProviderOrderNotFound) {
		t.Fatalf("err = %v, want provider order not found", err)
	}
	if err := sb.CloseOrder(t.Context(), "PWFMISSING"); err != nil {
		t.Fatalf("closing an unknown order must succeed: %v", err)
	}

	if _, err := sb.CreateOrder(t.Context(), OrderRequest{
		OutTradeNo: "PWFABC", Currency: "CNY", AmountCents: 50000,
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	state, err := sb.QueryOrder(t.Context(), "PWFABC")
	if err != nil || state.Status != OrderPending || state.AmountCents != 50000 || state.Currency != "CNY" {
		t.Fatalf("state = %+v, err = %v", state, err)
	}
	if err := sb.CloseOrder(t.Context(), "PWFABC"); err != nil {
		t.Fatalf("CloseOrder: %v", err)
	}
	if state, _ = sb.QueryOrder(t.Context(), "PWFABC"); state.Status != OrderClosed {
		t.Fatalf("status = %q, want closed", state.Status)
	}

	body := paymentEventBody(t, nil)
	if _, err := sb.VerifyCallback(callbackRequest(t, testSecret, time.Now(), body), body); err != nil {
		t.Fatalf("VerifyCallback: %v", err)
	}
	if state, _ = sb.QueryOrder(t.Context(), "PWFABC"); state.Status != OrderPaid || state.ProviderTradeNo != "wx-001" {
		t.Fatalf("state = %+v, want paid", state)
	}
	if err := sb.CloseOrder(t.Context(), "PWFABC"); !errors.Is(err, ErrProviderOrderPaid) {
		t.Fatalf("err = %v, want paid rejection", err)
	}
}

func TestVerifyCallbackValid(t *testing.T) {
	sb := newTestSandbox(t)
	body := paymentEventBody(t, nil)
	ev, err := sb.VerifyCallback(callbackRequest(t, testSecret, time.Now(), body), body)
	if err != nil {
		t.Fatalf("VerifyCallback: %v", err)
	}
	if ev.EventID != "evt-001" || ev.AmountCents != 50000 || ev.Currency != "CNY" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if len(ev.Raw) == 0 {
		t.Fatal("raw payload not preserved")
	}

	refundBody := paymentEventBody(t, func(ev map[string]any) {
		ev["event_id"] = "evt-refund-001"
		ev["type"] = EventRefundSucceeded
		ev["out_trade_no"] = ""
		ev["out_refund_no"] = "RFDXYZ"
		ev["provider_refund_id"] = "sbxr_001"
	})
	if _, err := sb.VerifyCallback(callbackRequest(t, testSecret, time.Now(), refundBody), refundBody); err != nil {
		t.Fatalf("refund callback: %v", err)
	}
}

func TestVerifyCallbackRejects(t *testing.T) {
	sb := newTestSandbox(t)
	now := time.Now()
	valid := paymentEventBody(t, nil)

	tampered := paymentEventBody(t, func(ev map[string]any) { ev["amount_cents"] = 1 })
	if _, err := sb.VerifyCallback(callbackRequest(t, testSecret, now, valid), tampered); err == nil {
		t.Fatal("expected tampered body to be rejected")
	}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/payment/sandbox", bytes.NewReader(valid))
	if _, err := sb.VerifyCallback(req, valid); err == nil {
		t.Fatal("expected missing signature to be rejected")
	}

	stale := callbackRequest(t, testSecret, now.Add(-10*time.Minute), valid)
	if _, err := sb.VerifyCallback(stale, valid); err == nil {
		t.Fatal("expected stale timestamp to be rejected")
	}
	future := callbackRequest(t, testSecret, now.Add(10*time.Minute), valid)
	if _, err := sb.VerifyCallback(future, valid); err == nil {
		t.Fatal("expected future timestamp to be rejected")
	}

	wrongMerchant := paymentEventBody(t, func(ev map[string]any) { ev["merchant_id"] = "merchant-999" })
	if _, err := sb.VerifyCallback(callbackRequest(t, testSecret, now, wrongMerchant), wrongMerchant); err == nil {
		t.Fatal("expected merchant mismatch to be rejected")
	}
}

func TestVerifyCallbackMalformed(t *testing.T) {
	sb := newTestSandbox(t)
	now := time.Now()
	cases := map[string]func(map[string]any){
		"unknown type":     func(ev map[string]any) { ev["type"] = "payment.refunded" },
		"missing event id": func(ev map[string]any) { ev["event_id"] = "" },
		"missing trade no": func(ev map[string]any) { ev["out_trade_no"] = "" },
		"non-positive":     func(ev map[string]any) { ev["amount_cents"] = 0 },
		"short currency":   func(ev map[string]any) { ev["currency"] = "CN" },
	}
	for name, mutate := range cases {
		body := paymentEventBody(t, mutate)
		req := callbackRequest(t, testSecret, now, body)
		if _, err := sb.VerifyCallback(req, body); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}

	notJSON := []byte(`{"event_id":`)
	req := callbackRequest(t, testSecret, now, notJSON)
	if _, err := sb.VerifyCallback(req, notJSON); err == nil {
		t.Fatal("expected invalid json to be rejected")
	}
}
