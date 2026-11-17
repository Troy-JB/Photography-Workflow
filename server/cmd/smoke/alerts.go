package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/alert"
)

type alertRecord struct {
	ID              uuid.UUID  `json:"id"`
	ProjectID       *uuid.UUID `json:"project_id"`
	Code            string     `json:"code"`
	Severity        string     `json:"severity"`
	Status          string     `json:"status"`
	NotifyStatus    string     `json:"notify_status"`
	NotifyAttempts  int        `json:"notify_attempts"`
	Occurrences     int        `json:"occurrences"`
	ResolutionNote  string     `json:"resolution_note"`
	AcknowledgedAt  *time.Time `json:"acknowledged_at"`
	ResolvedAt      *time.Time `json:"resolved_at"`
	LastNotifyError string     `json:"last_notify_error"`
}

type alertList struct {
	Total int           `json:"total"`
	Items []alertRecord `json:"items"`
}

// notificationWebhook is the reminder payload delivered by the worker. The
// receiver verifies it with the reminder secret, mirroring payment alerts.
type notificationWebhook struct {
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
	HeaderType     string          `json:"-"`
}

// alertCapture is the smoke-side webhook receiver. It verifies the HMAC
// signature with the production verifier so the delivery contract is exercised
// end to end. Alerts and reminders share the port and pick their secret by
// event type.
type alertCapture struct {
	mu            sync.Mutex
	notifications []alert.Notification
	reminders     []notificationWebhook
	failures      []string
}

func startAlertListener(addr, secret, reminderSecret string) (*alertCapture, func() error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	capture := &alertCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var probe struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(body, &probe)
		isReminder := probe.Event == "notification" || r.Header.Get("X-PWF-Event") == "notification"
		key := secret
		if isReminder {
			if reminderSecret == "" {
				capture.fail("reminder received but no reminder secret is configured")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			key = reminderSecret
		}
		if err := alert.VerifySignature(key, r.Header.Get("X-PWF-Webhook-Signature"),
			body, time.Now(), 5*time.Minute); err != nil {
			capture.fail(fmt.Sprintf("signature rejected: %v", err))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if isReminder {
			var n notificationWebhook
			if err := json.Unmarshal(body, &n); err != nil {
				capture.fail(fmt.Sprintf("bad reminder payload: %v", err))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if header := r.Header.Get("X-PWF-Event"); header != "" && header != n.Event {
				capture.fail(fmt.Sprintf("event header %q does not match body %q", header, n.Event))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n.HeaderType = r.Header.Get("X-PWF-Notification-Type")
			capture.addReminder(n)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var n alert.Notification
		if err := json.Unmarshal(body, &n); err != nil {
			capture.fail(fmt.Sprintf("bad payload: %v", err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		capture.add(n)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	stop := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
	return capture, stop, nil
}

func (c *alertCapture) add(n alert.Notification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifications = append(c.notifications, n)
}

func (c *alertCapture) addReminder(n notificationWebhook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reminders = append(c.reminders, n)
}

func (c *alertCapture) fail(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = append(c.failures, reason)
}

func (c *alertCapture) waitFor(alertID uuid.UUID, timeout time.Duration) (*alert.Notification, error) {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		var found *alert.Notification
		for i := range c.notifications {
			if c.notifications[i].AlertID == alertID {
				n := c.notifications[i]
				found = &n
				break
			}
		}
		failures := append([]string(nil), c.failures...)
		c.mu.Unlock()
		if len(failures) > 0 {
			return nil, fmt.Errorf("alert listener rejected a webhook: %v", failures)
		}
		if found != nil {
			return found, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("alert %s was not delivered to the webhook within %s", alertID, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *alertCapture) waitForReminder(notificationID uuid.UUID, timeout time.Duration) (*notificationWebhook, error) {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		var found *notificationWebhook
		for i := range c.reminders {
			if c.reminders[i].NotificationID == notificationID {
				n := c.reminders[i]
				found = &n
				break
			}
		}
		failures := append([]string(nil), c.failures...)
		c.mu.Unlock()
		if len(failures) > 0 {
			return nil, fmt.Errorf("webhook listener rejected a delivery: %v", failures)
		}
		if found != nil {
			return found, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("notification %s was not delivered to the webhook within %s", notificationID, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runAlerts verifies the security findings produced by the payment loop, walks
// the acknowledge/resolve lifecycle and validates the signed webhook delivery.
func runAlerts(c *client, cfg smokeConfig, projectID uuid.UUID, tag string) error {
	step("M2 payment alerts")
	list, err := c.listAlerts("/api/v1/payment-alerts?project_id=" + projectID.String() + "&limit=100")
	if err != nil {
		return err
	}
	expected := map[string]bool{
		"callback_amount_mismatch": false,
		"late_payment":             false,
		"close_after_paid":         false,
		"reconcile_mismatch":       false,
	}
	byCode := map[string]alertRecord{}
	for _, a := range list.Items {
		if _, ok := expected[a.Code]; ok {
			expected[a.Code] = true
			byCode[a.Code] = a
		}
	}
	for code, seen := range expected {
		if !seen {
			return fmt.Errorf("expected a %s alert for project %s; got %d alerts", code, projectID, list.Total)
		}
	}
	for _, code := range []string{"callback_amount_mismatch", "late_payment"} {
		if byCode[code].Severity != "critical" {
			return fmt.Errorf("alert %s severity = %s, want critical", code, byCode[code].Severity)
		}
	}
	foreign, err := c.listAlerts("/api/v1/payment-alerts?code=merchant_mismatch&limit=100")
	if err != nil {
		return err
	}
	if foreign.Total == 0 {
		return fmt.Errorf("foreign merchant callback did not raise a merchant_mismatch alert")
	}
	var metrics string
	if err := c.doText("/metrics", &metrics); err != nil {
		return err
	}
	for _, want := range []string{
		`payment_alerts_total{code="callback_amount_mismatch",severity="critical"}`,
		`payment_security_events_total{code="merchant_mismatch"}`,
	} {
		if !strings.Contains(metrics, want) {
			return fmt.Errorf("metrics missing %s", want)
		}
	}
	fmt.Printf("  project alerts=%d (amount/late/close/reconcile), merchant_mismatch=%d\n",
		list.Total, foreign.Total)

	step("M2 alert triage")
	target := byCode["callback_amount_mismatch"]
	var acked alertRecord
	if err := c.do(http.MethodPost, "/api/v1/payment-alerts/"+target.ID.String()+"/acknowledge", c.token, nil, &acked); err != nil {
		return err
	}
	if acked.Status != "acknowledged" || acked.AcknowledgedAt == nil {
		return fmt.Errorf("acknowledged alert = %+v", acked)
	}
	if err := c.expect(http.MethodPost, "/api/v1/payment-alerts/"+target.ID.String()+"/acknowledge",
		c.token, nil, 409, "alert_not_open"); err != nil {
		return err
	}
	var resolved alertRecord
	if err := c.do(http.MethodPost, "/api/v1/payment-alerts/"+target.ID.String()+"/resolve", c.token,
		map[string]any{"note": "smoke triage: rejected callback reviewed"}, &resolved); err != nil {
		return err
	}
	if resolved.Status != "resolved" || resolved.ResolvedAt == nil || resolved.ResolutionNote == "" {
		return fmt.Errorf("resolved alert = %+v", resolved)
	}
	if err := c.expect(http.MethodPost, "/api/v1/payment-alerts/"+target.ID.String()+"/resolve",
		c.token, map[string]any{"note": "again"}, 409, "alert_not_open"); err != nil {
		return err
	}
	var fetched alertRecord
	if err := c.do(http.MethodGet, "/api/v1/payment-alerts/"+target.ID.String(), c.token, nil, &fetched); err != nil {
		return err
	}
	if fetched.Status != "resolved" {
		return fmt.Errorf("alert after resolve = %s", fetched.Status)
	}
	fmt.Println("  acknowledge -> resolve, repeated act -> 409 alert_not_open, get returns resolved")

	step("M2 alert webhook")
	if c.alerts == nil {
		fmt.Println("  skipped (no -alert-listener)")
		if err := runScheduling(c, cfg, tag); err != nil {
			return err
		}
		if err := runPrep(c, cfg, tag); err != nil {
			return err
		}
		return runChangeOrders(c, cfg, projectID, tag)
	}
	note, err := c.alerts.waitFor(target.ID, 20*time.Second)
	if err != nil {
		return err
	}
	if note.Event != "payment_alert" || note.Code != target.Code || note.TenantID == uuid.Nil {
		return fmt.Errorf("webhook payload = %+v", note)
	}
	delivered, err := c.waitAlertNotify(target.ID, "sent", 20*time.Second)
	if err != nil {
		return err
	}
	if delivered.NotifyAttempts < 1 {
		return fmt.Errorf("alert notify_attempts = %d, want >= 1", delivered.NotifyAttempts)
	}
	fmt.Printf("  signed webhook accepted for %s, notify_status=%s attempts=%d\n",
		target.Code, delivered.NotifyStatus, delivered.NotifyAttempts)
	if err := runScheduling(c, cfg, tag); err != nil {
		return err
	}
	if err := runPrep(c, cfg, tag); err != nil {
		return err
	}
	return runChangeOrders(c, cfg, projectID, tag)
}

func (c *client) listAlerts(path string) (*alertList, error) {
	var list alertList
	if err := c.do(http.MethodGet, path, c.token, nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *client) getAlert(id uuid.UUID) (*alertRecord, error) {
	var a alertRecord
	if err := c.do(http.MethodGet, "/api/v1/payment-alerts/"+id.String(), c.token, nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (c *client) waitAlertNotify(id uuid.UUID, want string, timeout time.Duration) (*alertRecord, error) {
	deadline := time.Now().Add(timeout)
	var last *alertRecord
	for {
		a, err := c.getAlert(id)
		if err != nil {
			return nil, err
		}
		last = a
		if a.NotifyStatus == want {
			return a, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("alert %s notify_status = %s after %s, want %s (last error: %s)",
				id, last.NotifyStatus, timeout, want, last.LastNotifyError)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
