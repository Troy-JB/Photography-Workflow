package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/webhook"
)

// ErrPermanentDelivery marks a receiver answer that retrying cannot fix (4xx).
var ErrPermanentDelivery = webhook.ErrPermanentDelivery

// Notification is the webhook payload. It is intentionally self-contained so a
// receiver does not need to call back into the API to triage an alert.
type Notification struct {
	Event       string          `json:"event"`
	AlertID     uuid.UUID       `json:"alert_id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	ProjectID   *uuid.UUID      `json:"project_id,omitempty"`
	Code        string          `json:"code"`
	Severity    string          `json:"severity"`
	Status      string          `json:"status"`
	Title       string          `json:"title"`
	Detail      json.RawMessage `json:"detail"`
	AmountCents *int64          `json:"amount_cents,omitempty"`
	Occurrences int             `json:"occurrences"`
	FirstSeenAt time.Time       `json:"first_seen_at"`
	LastSeenAt  time.Time       `json:"last_seen_at"`
}

func NewNotification(a *Alert) Notification {
	return Notification{
		Event:       "payment_alert",
		AlertID:     a.ID,
		TenantID:    a.TenantID,
		ProjectID:   a.ProjectID,
		Code:        a.Code,
		Severity:    a.Severity,
		Status:      a.Status,
		Title:       a.Title,
		Detail:      a.Detail,
		AmountCents: a.AmountCents,
		Occurrences: a.Occurrences,
		FirstSeenAt: a.FirstSeenAt,
		LastSeenAt:  a.LastSeenAt,
	}
}

// SendWebhook delivers one notification. Network errors and 5xx answers are
// retryable; a 4xx answer wraps ErrPermanentDelivery so the worker stops.
func SendWebhook(ctx context.Context, client *http.Client, cfg Config, n Notification) error {
	if cfg.WebhookURL == "" {
		return fmt.Errorf("alert webhook url is not configured")
	}
	body, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("marshal alert notification: %w", err)
	}
	at := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build alert webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", n.AlertID.String())
	req.Header.Set("X-PWF-Event", "payment_alert")
	req.Header.Set("X-PWF-Webhook-Timestamp", strconv.FormatInt(at.Unix(), 10))
	req.Header.Set("X-PWF-Webhook-Signature", webhook.Signature(cfg.WebhookSecret, at, body))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post alert webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: status %d", ErrPermanentDelivery, resp.StatusCode)
	default:
		return fmt.Errorf("alert webhook returned status %d", resp.StatusCode)
	}
}

// Signature keeps the alert API stable while the implementation lives in the
// shared webhook package.
func Signature(secret string, at time.Time, body []byte) string {
	return webhook.Signature(secret, at, body)
}

// VerifySignature validates a webhook signature header against the raw body and
// the timestamp tolerance. Receivers (including the smoke listener) use it.
func VerifySignature(secret, header string, body []byte, at time.Time, tolerance time.Duration) error {
	return webhook.VerifySignature(secret, header, body, at, tolerance)
}
