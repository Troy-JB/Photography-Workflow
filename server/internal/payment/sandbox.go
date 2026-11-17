package payment

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sandbox is the development provider. It does not talk to WeChat Pay or
// Alipay: it issues local pay URLs, authenticates callbacks with HMAC-SHA256
// and settles refunds asynchronously through a callback, so the full
// order -> pay -> refund path (amount checks, replay protection, projections)
// can be exercised without merchant credentials. Production adapters implement
// the same Provider contract.
type Sandbox struct {
	secret     []byte
	merchantID string
	payBase    string
	tolerance  time.Duration
	now        func() time.Time

	mu     sync.Mutex
	orders map[string]*sandboxOrder
}

// sandboxOrder mirrors the channel-side order state so QueryOrder and
// CloseOrder behave like a real provider inside one process.
type sandboxOrder struct {
	amountCents     int64
	currency        string
	status          string
	providerTradeNo string
	paidAt          *time.Time
}

func NewSandbox(secret, merchantID, payBaseURL string, tolerance time.Duration) (*Sandbox, error) {
	if len(secret) < 16 {
		return nil, fmt.Errorf("payment callback secret must be at least 16 characters")
	}
	merchantID = strings.TrimSpace(merchantID)
	if merchantID == "" {
		return nil, fmt.Errorf("payment.merchant_id is required for the sandbox provider")
	}
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}
	payBaseURL = strings.TrimRight(strings.TrimSpace(payBaseURL), "/")
	if payBaseURL == "" {
		return nil, fmt.Errorf("payment.pay_base_url is required for the sandbox provider")
	}
	return &Sandbox{
		secret:     []byte(secret),
		merchantID: merchantID,
		payBase:    payBaseURL,
		tolerance:  tolerance,
		now:        time.Now,
		orders:     make(map[string]*sandboxOrder),
	}, nil
}

func (s *Sandbox) Name() string { return "sandbox" }

func (s *Sandbox) MerchantID() string { return s.merchantID }

func (s *Sandbox) CreateOrder(_ context.Context, req OrderRequest) (*OrderParams, error) {
	if strings.TrimSpace(req.OutTradeNo) == "" {
		return nil, fmt.Errorf("out_trade_no is required")
	}
	if req.AmountCents <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	if len(req.Currency) != 3 {
		return nil, fmt.Errorf("currency must be a 3-letter code")
	}
	id, err := randomHex(10)
	if err != nil {
		return nil, err
	}
	token, err := randomHex(18)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.orders[req.OutTradeNo] = &sandboxOrder{
		amountCents: req.AmountCents,
		currency:    req.Currency,
		status:      OrderPending,
	}
	s.mu.Unlock()
	return &OrderParams{
		ProviderOrderID: "sbxo_" + id,
		PayURL:          s.payBase + "/" + req.OutTradeNo + "?token=" + token,
	}, nil
}

func (s *Sandbox) QueryOrder(_ context.Context, outTradeNo string) (*OrderState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[outTradeNo]
	if !ok {
		return nil, ErrProviderOrderNotFound
	}
	return &OrderState{
		OutTradeNo:      outTradeNo,
		Status:          order.status,
		ProviderTradeNo: order.providerTradeNo,
		AmountCents:     order.amountCents,
		Currency:        order.currency,
		PaidAt:          order.paidAt,
	}, nil
}

// CompleteOrder is only used by the authenticated development checkout flow.
// It updates the provider-side sandbox state and returns a normal callback so
// all payment validation, idempotency and projections still use HandleCallback.
func (s *Sandbox) CompleteOrder(outTradeNo string) (*CallbackEvent, error) {
	s.mu.Lock()
	order, ok := s.orders[outTradeNo]
	if !ok || order.status != OrderPending {
		s.mu.Unlock()
		return nil, ErrOrderNotPending
	}
	tradeNo, err := randomHex(10)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	paidAt := s.now().UTC()
	order.status = OrderPaid
	order.providerTradeNo = "sandbox_" + tradeNo
	order.paidAt = &paidAt
	ev := &CallbackEvent{
		EventID: "sandbox_" + tradeNo, Type: EventPaymentSucceeded, MerchantID: s.merchantID,
		OutTradeNo: outTradeNo, ProviderTradeNo: order.providerTradeNo,
		AmountCents: order.amountCents, Currency: order.currency, PaidAt: &paidAt,
	}
	s.mu.Unlock()
	ev.Raw, err = json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return ev, nil
}

func (s *Sandbox) CloseOrder(_ context.Context, outTradeNo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	order, ok := s.orders[outTradeNo]
	if !ok || order.status == OrderClosed {
		return nil
	}
	if order.status == OrderPaid {
		return ErrProviderOrderPaid
	}
	order.status = OrderClosed
	return nil
}

func (s *Sandbox) Refund(_ context.Context, req RefundRequest) (*RefundResult, error) {
	if strings.TrimSpace(req.OutRefundNo) == "" {
		return nil, fmt.Errorf("out_refund_no is required")
	}
	if req.AmountCents <= 0 {
		return nil, fmt.Errorf("refund amount must be positive")
	}
	id, err := randomHex(10)
	if err != nil {
		return nil, err
	}
	// Real providers settle refunds asynchronously; the final state arrives as
	// a refund callback so the callback path stays exercised.
	return &RefundResult{ProviderRefundID: "sbxr_" + id, Status: RefundPending}, nil
}

func (s *Sandbox) VerifyCallback(r *http.Request, body []byte) (*CallbackEvent, error) {
	ts, sig, err := parseSignatureHeader(r.Header.Get("X-PWF-Signature"))
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.Sub(ts) > s.tolerance || ts.Sub(now) > s.tolerance {
		return nil, fmt.Errorf("%w: timestamp outside tolerance", ErrInvalidSignature)
	}
	expected := sign(s.secret, ts, body)
	if !hmac.Equal(expected, sig) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrInvalidSignature)
	}

	var ev CallbackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedCallback, err)
	}
	ev.Raw = append(json.RawMessage(nil), body...)
	if err := ValidateEvent(&ev); err != nil {
		return nil, err
	}
	if ev.MerchantID != s.merchantID {
		return nil, fmt.Errorf("%w: merchant id mismatch", ErrMerchantMismatch)
	}
	if ev.Type == EventPaymentSucceeded {
		s.mu.Lock()
		if order, ok := s.orders[ev.OutTradeNo]; ok {
			order.status = OrderPaid
			if ev.ProviderTradeNo != "" {
				order.providerTradeNo = ev.ProviderTradeNo
			}
			paidAt := s.now().UTC()
			if ev.PaidAt != nil {
				paidAt = ev.PaidAt.UTC()
			}
			order.paidAt = &paidAt
		}
		s.mu.Unlock()
	}
	return &ev, nil
}

// SignRequestBody is used by sandbox tooling and smoke tests to produce a
// callback that VerifyCallback accepts.
func SignRequestBody(secret string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(sign([]byte(secret), at, body))
}

func sign(secret []byte, at time.Time, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(at.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}

func parseSignatureHeader(header string) (time.Time, []byte, error) {
	var (
		hasTS bool
		ts    int64
		sig   []byte
	)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			v, err := strconv.ParseInt(strings.TrimPrefix(part, "t="), 10, 64)
			if err != nil {
				return time.Time{}, nil, fmt.Errorf("%w: bad timestamp", ErrInvalidSignature)
			}
			ts, hasTS = v, true
		case strings.HasPrefix(part, "v1="):
			raw, err := hex.DecodeString(strings.TrimPrefix(part, "v1="))
			if err != nil || len(raw) != sha256.Size {
				return time.Time{}, nil, fmt.Errorf("%w: bad signature value", ErrInvalidSignature)
			}
			sig = raw
		}
	}
	if !hasTS || sig == nil {
		return time.Time{}, nil, fmt.Errorf("%w: missing t or v1", ErrInvalidSignature)
	}
	return time.Unix(ts, 0), sig, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
