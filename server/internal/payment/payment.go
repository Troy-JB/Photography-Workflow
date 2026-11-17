package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	ErrInvalidSignature  = errors.New("invalid callback signature")
	ErrMalformedCallback = errors.New("malformed callback")
	// ErrMerchantMismatch means the callback signature is authentic but the
	// payload belongs to another merchant account.
	ErrMerchantMismatch = errors.New("callback merchant id mismatch")
	// ErrProviderOrderPaid means the channel already captured the payment, so
	// closing the order would strand customer funds.
	ErrProviderOrderPaid = errors.New("payment order already paid at provider")
	// ErrProviderOrderNotFound means the channel has no record of the order.
	ErrProviderOrderNotFound = errors.New("payment order not found at provider")
)

const (
	EventPaymentSucceeded = "payment.succeeded"
	EventRefundSucceeded  = "refund.succeeded"
	EventRefundFailed     = "refund.failed"
)

type OrderRequest struct {
	OutTradeNo  string
	Description string
	Currency    string
	AmountCents int64
	ExpireAt    time.Time
	ClientIP    string
}

type OrderParams struct {
	ProviderOrderID string
	PayURL          string
}

type RefundRequest struct {
	OutTradeNo  string
	OutRefundNo string
	Reason      string
	Currency    string
	AmountCents int64
}

// RefundResult is either final ("succeeded") or accepted and awaiting a
// provider callback ("pending").
type RefundResult struct {
	ProviderRefundID string
	Status           string
}

// CallbackEvent is the normalised payload every provider adapter produces.
// Raw keeps the original provider payload for the evidence trail.
type CallbackEvent struct {
	EventID          string          `json:"event_id"`
	Type             string          `json:"type"`
	MerchantID       string          `json:"merchant_id"`
	OutTradeNo       string          `json:"out_trade_no,omitempty"`
	OutRefundNo      string          `json:"out_refund_no,omitempty"`
	ProviderTradeNo  string          `json:"provider_trade_no,omitempty"`
	ProviderRefundID string          `json:"provider_refund_id,omitempty"`
	AmountCents      int64           `json:"amount_cents"`
	Currency         string          `json:"currency"`
	PaidAt           *time.Time      `json:"paid_at,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	Raw              json.RawMessage `json:"-"`
}

// OrderState is the channel's view of an order. Reconcile compares it with the
// local order before crediting; CloseOrder uses Status to detect the
// paid-at-close race.
type OrderState struct {
	OutTradeNo      string
	Status          string // pending | paid | closed
	ProviderTradeNo string
	AmountCents     int64
	Currency        string
	PaidAt          *time.Time
}

type Provider interface {
	Name() string
	CreateOrder(ctx context.Context, req OrderRequest) (*OrderParams, error)
	// VerifyCallback authenticates the request before the payload is trusted.
	// Implementations must reject stale timestamps, verify the merchant id and
	// compare signatures in constant time.
	VerifyCallback(r *http.Request, body []byte) (*CallbackEvent, error)
	// Refund initiates a refund. Providers may settle asynchronously; the
	// returned status is "succeeded" or "pending" and the final state arrives
	// through a refund callback.
	Refund(ctx context.Context, req RefundRequest) (*RefundResult, error)
	// QueryOrder returns the channel's authoritative state. Reconciliation and
	// orphan recovery rely on it when a callback was lost.
	QueryOrder(ctx context.Context, outTradeNo string) (*OrderState, error)
	// CloseOrder closes the order at the channel before the local row is
	// closed, so a customer can no longer pay it. Closing an unknown or
	// already-closed order must succeed; a captured payment must return
	// ErrProviderOrderPaid.
	CloseOrder(ctx context.Context, outTradeNo string) error
}

func ValidateEvent(ev *CallbackEvent) error {
	if ev.EventID == "" || len(ev.EventID) > 128 {
		return fmt.Errorf("%w: event_id is required and must be at most 128 chars", ErrMalformedCallback)
	}
	switch ev.Type {
	case EventPaymentSucceeded:
		if ev.OutTradeNo == "" || len(ev.OutTradeNo) > 64 {
			return fmt.Errorf("%w: out_trade_no is required", ErrMalformedCallback)
		}
	case EventRefundSucceeded, EventRefundFailed:
		if ev.OutRefundNo == "" || len(ev.OutRefundNo) > 64 {
			return fmt.Errorf("%w: out_refund_no is required", ErrMalformedCallback)
		}
	default:
		return fmt.Errorf("%w: unknown event type %q", ErrMalformedCallback, ev.Type)
	}
	if ev.AmountCents <= 0 {
		return fmt.Errorf("%w: amount_cents must be positive", ErrMalformedCallback)
	}
	if len(ev.Currency) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter code", ErrMalformedCallback)
	}
	return nil
}
