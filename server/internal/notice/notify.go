package notice

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

// WebhookNotification is the self-contained payload delivered to a configured
// reminder receiver. The in-app row remains the source of truth.
type WebhookNotification struct {
	Event          string          `json:"event"`
	NotificationID uuid.UUID       `json:"notification_id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	Type           string          `json:"type"`
	SubjectType    string          `json:"subject_type"`
	SubjectID      uuid.UUID       `json:"subject_id"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	Payload        json.RawMessage `json:"payload"`
	DueAt          time.Time       `json:"due_at"`
	CreatedAt      time.Time       `json:"created_at"`
}

func NewWebhookNotification(n *Notification) WebhookNotification {
	return WebhookNotification{
		Event:          "notification",
		NotificationID: n.ID,
		TenantID:       n.TenantID,
		Type:           n.Type,
		SubjectType:    n.SubjectType,
		SubjectID:      n.SubjectID,
		Title:          n.Title,
		Body:           n.Body,
		Payload:        n.Payload,
		DueAt:          n.DueAt,
		CreatedAt:      n.CreatedAt,
	}
}

// Applicable reports whether a reminder still describes its subject. Booking
// reminders go stale when the booking is cancelled, expired or rescheduled.
// Other subject types (gallery events) carry no time-sensitive precondition:
// the pending row is its own source of truth, and deleting the subject cascades
// the notification away.
func Applicable(n *Notification, state *DeliveryState, now time.Time) bool {
	if n.SubjectType != "booking" {
		return true
	}
	if state == nil || !state.Found {
		return false
	}
	switch n.Type {
	case TypeShootReminder:
		return state.BookingStatus == "confirmed" && state.StartsAt.After(now)
	case TypeHoldExpiry:
		return state.BookingStatus == "held" && state.HoldExpiresAt != nil &&
			state.HoldExpiresAt.After(now)
	case TypeAuthIncomplete:
		return state.BookingStatus == "confirmed" && state.StartsAt.After(now) &&
			state.PendingAuths > 0
	}
	return false
}

// Deliver sends one webhook. Status is one of sent/skipped; an error means the
// job should retry, except ErrPermanentDelivery which is terminal for the
// notification. It performs no database writes so the worker controls ordering.
func (s *Service) Deliver(ctx context.Context, client *http.Client, n *Notification, state *DeliveryState, now time.Time) (string, error) {
	if !s.cfg.Enabled || s.cfg.WebhookURL == "" {
		return StatusSkipped, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !Applicable(n, state, now) {
		return StatusSkipped, nil
	}
	body, err := json.Marshal(NewWebhookNotification(n))
	if err != nil {
		return "", fmt.Errorf("marshal reminder notification: %w", err)
	}
	at := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build reminder webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", n.ID.String())
	req.Header.Set("X-PWF-Event", "notification")
	req.Header.Set("X-PWF-Notification-Type", n.Type)
	req.Header.Set("X-PWF-Webhook-Timestamp", strconv.FormatInt(at.Unix(), 10))
	req.Header.Set("X-PWF-Webhook-Signature", webhook.Signature(s.cfg.WebhookSecret, at, body))

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("post reminder webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return StatusSent, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "", fmt.Errorf("%w: status %d", webhook.ErrPermanentDelivery, resp.StatusCode)
	default:
		return "", fmt.Errorf("reminder webhook returned status %d", resp.StatusCode)
	}
}
