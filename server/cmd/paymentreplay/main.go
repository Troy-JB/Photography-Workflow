package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"photography-workflow/server/internal/payment"
)

type config struct {
	Base           string
	Provider       string
	Secret         string
	Merchant       string
	Scenario       string
	OutTradeNo     string
	AmountCents    int64
	OutRefundNo    string
	RefundCents    int64
	ProviderTrade  string
	ProviderRefund string
	EventID        string
	Count          int
	Concurrent     int
	DeliveryToken  string
	Email          string
	Password       string
	ProjectID      string
	PaymentID      string
	AllowRemote    bool
	JSONOut        string
}

type callbackReply struct {
	status int
	result string
	code   string
	raw    string
	err    error
}

type phaseSummary struct {
	Requests   int            `json:"requests"`
	Processed  int            `json:"processed"`
	Duplicates int            `json:"duplicates"`
	Ignored    int            `json:"ignored"`
	Rejected   int            `json:"rejected"`
	Failed     int            `json:"failed"`
	Codes      map[string]int `json:"codes"`
}

type replayReport struct {
	Scenario     string         `json:"scenario"`
	Base         string         `json:"base"`
	EventID      string         `json:"event_id"`
	OutTradeNo   string         `json:"out_trade_no,omitempty"`
	OutRefundNo  string         `json:"out_refund_no,omitempty"`
	Sequential   phaseSummary   `json:"sequential"`
	Concurrent   phaseSummary   `json:"concurrent"`
	Processed    int            `json:"processed_total"`
	DeliveryGate map[string]any `json:"delivery_gate,omitempty"`
	PaymentCount *int           `json:"payments_for_project,omitempty"`
	RefundCount  *int           `json:"succeeded_refunds,omitempty"`
	Failures     []string       `json:"failures,omitempty"`
	Passed       bool           `json:"passed"`
}

func main() {
	var cfg config
	flag.StringVar(&cfg.Base, "base", envOr("PWF_REPLAY_BASE", "http://127.0.0.1:8080"), "API base URL")
	flag.StringVar(&cfg.Provider, "provider", "sandbox", "payment provider path segment")
	flag.StringVar(&cfg.Secret, "secret", envOr("PWF_PAYMENT__CALLBACK_SECRET", "dev-only-payment-secret-change-me"),
		"sandbox callback HMAC secret (must match the API)")
	flag.StringVar(&cfg.Merchant, "merchant", envOr("PWF_PAYMENT__MERCHANT_ID", "sandbox-merchant-001"),
		"merchant id the events claim")
	flag.StringVar(&cfg.Scenario, "scenario", "payment", "payment | refund")
	flag.StringVar(&cfg.OutTradeNo, "out-trade-no", "", "order number being replayed")
	flag.Int64Var(&cfg.AmountCents, "amount-cents", 0, "payment amount in cents")
	flag.StringVar(&cfg.OutRefundNo, "out-refund-no", "", "refund number being replayed")
	flag.Int64Var(&cfg.RefundCents, "refund-amount-cents", 0, "refund amount in cents (defaults to -amount-cents)")
	flag.StringVar(&cfg.ProviderTrade, "provider-trade-no", "", "provider trade number to echo")
	flag.StringVar(&cfg.ProviderRefund, "provider-refund-id", "", "provider refund id to echo")
	flag.StringVar(&cfg.EventID, "event-id", "", "event id replayed (defaults to a stable generated id)")
	flag.IntVar(&cfg.Count, "count", 100, "replays per phase (sequential and concurrent)")
	flag.IntVar(&cfg.Concurrent, "concurrent", 100, "parallel replays in the concurrent phase")
	flag.StringVar(&cfg.DeliveryToken, "delivery-token", "", "optional link token to prove the refund gate returns 409 payment_required")
	flag.StringVar(&cfg.Email, "email", "owner@example.com", "login email for optional state checks")
	flag.StringVar(&cfg.Password, "password", "photographer123", "login password for optional state checks")
	flag.StringVar(&cfg.ProjectID, "project", "", "optional project id: assert exactly one credited payment afterwards")
	flag.StringVar(&cfg.PaymentID, "payment-id", "", "optional payment id: assert exactly one succeeded refund afterwards")
	flag.BoolVar(&cfg.AllowRemote, "allow-remote", false,
		"allow a non-loopback target; never point this at a production channel with real money")
	flag.StringVar(&cfg.JSONOut, "json-out", "", "optional path for a machine-readable report")
	flag.Parse()

	if err := validate(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "paymentreplay: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rep, err := replay(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "paymentreplay: %v\n", err)
		os.Exit(1)
	}
	rep.print()
	if err := writeReport(cfg.JSONOut, rep); err != nil {
		fmt.Fprintf(os.Stderr, "paymentreplay: write %s: %v\n", cfg.JSONOut, err)
		os.Exit(1)
	}
	if !rep.Passed {
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func validate(cfg *config) error {
	u, err := url.Parse(cfg.Base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("-base must be an absolute http(s) URL, got %q", cfg.Base)
	}
	if !cfg.AllowRemote && !isLoopback(u.Hostname()) {
		return fmt.Errorf("refusing to replay against %s: pass -allow-remote only for a sandbox channel", u.Hostname())
	}
	if strings.TrimSpace(cfg.Secret) == "" {
		return fmt.Errorf("-secret is required")
	}
	if cfg.Count < 1 || cfg.Count > 10_000 {
		return fmt.Errorf("-count must be between 1 and 10000")
	}
	if cfg.Concurrent < 1 || cfg.Concurrent > cfg.Count {
		return fmt.Errorf("-concurrent must be between 1 and -count")
	}
	switch cfg.Scenario {
	case "payment":
		if cfg.OutTradeNo == "" || cfg.AmountCents <= 0 {
			return fmt.Errorf("payment replay needs -out-trade-no and a positive -amount-cents")
		}
	case "refund":
		if cfg.OutRefundNo == "" {
			return fmt.Errorf("refund replay needs -out-refund-no")
		}
		if cfg.RefundCents == 0 {
			cfg.RefundCents = cfg.AmountCents
		}
		if cfg.RefundCents <= 0 {
			return fmt.Errorf("refund replay needs a positive -refund-amount-cents")
		}
	default:
		return fmt.Errorf("-scenario must be payment or refund")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func replay(ctx context.Context, cfg config) (*replayReport, error) {
	rep := &replayReport{Scenario: cfg.Scenario, Base: cfg.Base}
	if cfg.EventID == "" {
		cfg.EventID = fmt.Sprintf("replay-%s-%s-%d", cfg.Scenario, time.Now().Format("150405"), time.Now().UnixNano()%1000)
	} else {
		// Keep the same id across both phases but suffix the scenario so payment
		// and refund runs never collide in the event log.
		cfg.EventID = cfg.EventID + "-" + cfg.Scenario
	}
	rep.EventID = cfg.EventID
	rep.OutTradeNo = cfg.OutTradeNo
	rep.OutRefundNo = cfg.OutRefundNo

	payload := eventPayload(cfg)
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	send := func(ctx context.Context) callbackReply {
		return postCallback(ctx, cfg, raw)
	}

	rep.Sequential = runPhase(ctx, cfg.Count, 1, send)
	rep.Concurrent = runPhase(ctx, cfg.Count, cfg.Concurrent, send)
	rep.Processed = rep.Sequential.Processed + rep.Concurrent.Processed

	if rep.Processed != 1 {
		rep.fail("event %s was processed %d times, want exactly 1", cfg.EventID, rep.Processed)
	}
	for _, phase := range []struct {
		name string
		sum  phaseSummary
	}{{"sequential", rep.Sequential}, {"concurrent", rep.Concurrent}} {
		if phase.sum.Failed > 0 {
			rep.fail("%s phase had %d failed requests (codes %v)", phase.name, phase.sum.Failed, phase.sum.Codes)
		}
		if phase.sum.Ignored > 0 {
			rep.fail("%s phase had %d ignored callbacks; check the order/refund number", phase.name, phase.sum.Ignored)
		}
		if phase.sum.Rejected > 0 {
			rep.fail("%s phase had %d rejected callbacks (codes %v)", phase.name, phase.sum.Rejected, phase.sum.Codes)
		}
	}

	if cfg.ProjectID != "" || cfg.PaymentID != "" {
		if err := verifyState(ctx, cfg, rep); err != nil {
			rep.fail("state check: %v", err)
		}
	}
	if cfg.Scenario == "refund" && cfg.DeliveryToken != "" {
		if err := verifyDeliveryGate(ctx, cfg, rep); err != nil {
			rep.fail("delivery gate: %v", err)
		}
	}
	rep.Passed = len(rep.Failures) == 0
	return rep, nil
}

func (rep *replayReport) fail(format string, args ...any) {
	rep.Failures = append(rep.Failures, fmt.Sprintf(format, args...))
}

func (rep *replayReport) print() {
	fmt.Printf("scenario      %s\n", rep.Scenario)
	fmt.Printf("event_id      %s\n", rep.EventID)
	fmt.Printf("sequential    %+v\n", rep.Sequential)
	fmt.Printf("concurrent    %+v\n", rep.Concurrent)
	fmt.Printf("processed     %d total (want exactly 1)\n", rep.Processed)
	for k, v := range rep.DeliveryGate {
		fmt.Printf("gate.%-8s %v\n", k, v)
	}
	if rep.PaymentCount != nil {
		fmt.Printf("payments      %d for the project (want 1)\n", *rep.PaymentCount)
	}
	if rep.RefundCount != nil {
		fmt.Printf("refunds       %d succeeded (want 1)\n", *rep.RefundCount)
	}
	if rep.Passed {
		fmt.Println("verdict       PASS")
		return
	}
	fmt.Println("verdict       FAIL")
	for _, f := range rep.Failures {
		fmt.Printf("  - %s\n", f)
	}
}

func eventPayload(cfg config) map[string]any {
	if cfg.Scenario == "refund" {
		ev := map[string]any{
			"event_id":      cfg.EventID,
			"type":          payment.EventRefundSucceeded,
			"merchant_id":   cfg.Merchant,
			"out_refund_no": cfg.OutRefundNo,
			"amount_cents":  cfg.RefundCents,
			"currency":      "CNY",
			"reason":        "payment replay drill",
		}
		if cfg.ProviderRefund != "" {
			ev["provider_refund_id"] = cfg.ProviderRefund
		}
		if cfg.OutTradeNo != "" {
			ev["out_trade_no"] = cfg.OutTradeNo
		}
		return ev
	}
	ev := map[string]any{
		"event_id":     cfg.EventID,
		"type":         payment.EventPaymentSucceeded,
		"merchant_id":  cfg.Merchant,
		"out_trade_no": cfg.OutTradeNo,
		"amount_cents": cfg.AmountCents,
		"currency":     "CNY",
		"paid_at":      time.Now().UTC().Format(time.RFC3339),
	}
	if cfg.ProviderTrade != "" {
		ev["provider_trade_no"] = cfg.ProviderTrade
	}
	return ev
}

func runPhase(ctx context.Context, count, parallel int, send func(context.Context) callbackReply) phaseSummary {
	sum := phaseSummary{Codes: map[string]int{}}
	results := make([]callbackReply, count)
	if parallel <= 1 {
		for i := 0; i < count; i++ {
			results[i] = send(ctx)
		}
	} else {
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := 0; i < count; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-gate
				results[i] = send(ctx)
			}(i)
		}
		close(gate)
		wg.Wait()
	}
	for _, res := range results {
		sum.Requests++
		switch {
		case res.err != nil:
			sum.Failed++
			sum.Codes["transport_error"]++
		case res.status >= 500:
			sum.Failed++
			sum.Codes[fmt.Sprintf("http_%d", res.status)]++
		case res.code != "":
			sum.Rejected++
			sum.Codes[res.code]++
		case res.result == "processed":
			sum.Processed++
		case res.result == "duplicate":
			sum.Duplicates++
		case res.result == "ignored":
			sum.Ignored++
			sum.Codes["ignored"]++
		default:
			sum.Failed++
			sum.Codes[fmt.Sprintf("status_%d", res.status)]++
		}
	}
	return sum
}

func postCallback(ctx context.Context, cfg config, body []byte) callbackReply {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.Base, "/")+"/webhooks/payment/"+cfg.Provider, bytes.NewReader(body))
	if err != nil {
		return callbackReply{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PWF-Signature", payment.SignRequestBody(cfg.Secret, time.Now(), body))
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return callbackReply{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return callbackReply{status: resp.StatusCode, err: err}
	}
	var parsed struct {
		Status string `json:"status"`
		Code   string `json:"code"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return callbackReply{status: resp.StatusCode, result: parsed.Status, code: parsed.Code, raw: string(raw)}
}

// verifyState uses the owner API to prove the replay left exactly one credited
// payment (or one succeeded refund) behind.
func verifyState(ctx context.Context, cfg config, rep *replayReport) error {
	token, err := login(ctx, cfg)
	if err != nil {
		return err
	}
	if cfg.ProjectID != "" {
		var list struct {
			Total int `json:"total"`
		}
		if err := apiJSON(ctx, cfg, token, http.MethodGet, "/api/v1/payments?project_id="+cfg.ProjectID, nil, &list); err != nil {
			return err
		}
		count := list.Total
		rep.PaymentCount = &count
		if count != 1 {
			return fmt.Errorf("project has %d payments after the replay, want 1", count)
		}
	}
	if cfg.PaymentID != "" {
		var list struct {
			Items []struct {
				Status string `json:"status"`
			} `json:"items"`
		}
		if err := apiJSON(ctx, cfg, token, http.MethodGet, "/api/v1/payments/"+cfg.PaymentID+"/refunds", nil, &list); err != nil {
			return err
		}
		succeeded := 0
		for _, item := range list.Items {
			if item.Status == "succeeded" {
				succeeded++
			}
		}
		rep.RefundCount = &succeeded
		if succeeded != 1 {
			return fmt.Errorf("payment has %d succeeded refunds, want 1", succeeded)
		}
	}
	return nil
}

func verifyDeliveryGate(ctx context.Context, cfg config, rep *replayReport) error {
	status, raw, err := apiCall(ctx, cfg, "", http.MethodPost, "/g/v1/delivery/session",
		map[string]any{"token": cfg.DeliveryToken})
	if err != nil {
		return err
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &problem)
	rep.DeliveryGate = map[string]any{"status": status, "code": problem.Code}
	if status != http.StatusConflict || problem.Code != "payment_required" {
		return fmt.Errorf("delivery session after refund returned %d %q, want 409 payment_required", status, problem.Code)
	}
	return nil
}

func login(ctx context.Context, cfg config) (string, error) {
	status, raw, err := apiCall(ctx, cfg, "", http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": cfg.Email, "password": cfg.Password})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("login: status %d: %s", status, truncate(raw, 200))
	}
	var session struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		return "", err
	}
	return session.AccessToken, nil
}

func apiJSON(ctx context.Context, cfg config, token, method, path string, body, out any) error {
	status, raw, err := apiCall(ctx, cfg, token, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, status, truncate(raw, 200))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func apiCall(ctx context.Context, cfg config, token, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cfg.Base, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, err
}

func truncate(raw []byte, limit int) string {
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "…"
}

func writeReport(path string, rep *replayReport) error {
	if path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
