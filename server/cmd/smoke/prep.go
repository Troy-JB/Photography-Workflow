package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/payment"
)

type prepShotItem struct {
	ID           uuid.UUID  `json:"id"`
	ShotListID   uuid.UUID  `json:"shot_list_id"`
	SortOrder    int        `json:"sort_order"`
	Category     string     `json:"category"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	ReferenceURL string     `json:"reference_url"`
	Priority     string     `json:"priority"`
	Done         bool       `json:"done"`
	DoneAt       *time.Time `json:"done_at"`
	Version      int        `json:"version"`
}

type prepShotList struct {
	ID        uuid.UUID      `json:"id"`
	BookingID uuid.UUID      `json:"booking_id"`
	Title     string         `json:"title"`
	Note      string         `json:"note"`
	Version   int            `json:"version"`
	Items     []prepShotItem `json:"items"`
}

type prepGear struct {
	ID           uuid.UUID  `json:"id"`
	BookingID    uuid.UUID  `json:"booking_id"`
	ResourceID   *uuid.UUID `json:"resource_id"`
	ResourceName string     `json:"resource_name"`
	Name         string     `json:"name"`
	Category     string     `json:"category"`
	Quantity     int        `json:"quantity"`
	Checked      bool       `json:"checked"`
	CheckedAt    *time.Time `json:"checked_at"`
	Version      int        `json:"version"`
}

type prepAuthorization struct {
	ID        uuid.UUID  `json:"id"`
	BookingID uuid.UUID  `json:"booking_id"`
	Kind      string     `json:"kind"`
	Subject   string     `json:"subject"`
	Status    string     `json:"status"`
	SignedAt  *time.Time `json:"signed_at"`
	Note      string     `json:"note"`
	Version   int        `json:"version"`
}

type prepCallSheet struct {
	ID             uuid.UUID       `json:"id"`
	BookingID      uuid.UUID       `json:"booking_id"`
	Version        int             `json:"version"`
	Snapshot       json.RawMessage `json:"snapshot"`
	SnapshotSHA256 string          `json:"snapshot_sha256"`
	CreatedAt      time.Time       `json:"created_at"`
}

type prepCallSheetSummary struct {
	ID             uuid.UUID `json:"id"`
	BookingID      uuid.UUID `json:"booking_id"`
	Version        int       `json:"version"`
	SnapshotSHA256 string    `json:"snapshot_sha256"`
}

type prepSnapshot struct {
	Schema  string `json:"schema"`
	Version int    `json:"version"`
	Booking struct {
		ID        uuid.UUID `json:"id"`
		StartsAt  time.Time `json:"starts_at"`
		EndsAt    time.Time `json:"ends_at"`
		Resources []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"resources"`
	} `json:"booking"`
	ShotList *struct {
		Items []struct {
			Title        string `json:"title"`
			ReferenceURL string `json:"reference_url"`
			Done         bool   `json:"done"`
		} `json:"items"`
	} `json:"shot_list"`
	Gear []struct {
		Name     string `json:"name"`
		Quantity int    `json:"quantity"`
		Checked  bool   `json:"checked"`
	} `json:"gear"`
	Authorizations []struct {
		Kind     string     `json:"kind"`
		Subject  string     `json:"subject"`
		Status   string     `json:"status"`
		SignedAt *time.Time `json:"signed_at"`
	} `json:"authorizations"`
}

type noticeDelivery struct {
	ID          uuid.UUID  `json:"id"`
	Channel     string     `json:"channel"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

type noticeItem struct {
	ID          uuid.UUID        `json:"id"`
	TenantID    uuid.UUID        `json:"tenant_id"`
	Type        string           `json:"type"`
	SubjectType string           `json:"subject_type"`
	SubjectID   uuid.UUID        `json:"subject_id"`
	DedupeKey   string           `json:"dedupe_key"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	Payload     json.RawMessage  `json:"payload"`
	Status      string           `json:"status"`
	DueAt       time.Time        `json:"due_at"`
	SentAt      *time.Time       `json:"sent_at"`
	ReadAt      *time.Time       `json:"read_at"`
	Attempts    int              `json:"attempts"`
	LastError   string           `json:"last_error"`
	Deliveries  []noticeDelivery `json:"deliveries"`
}

type noticeList struct {
	Items  []noticeItem `json:"items"`
	Total  int          `json:"total"`
	Unread int          `json:"unread"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// runPrep exercises the M3 pre-shoot scope: shot list, gear, authorizations,
// frozen call sheets with stable hashes, and planning/delivery of reminders.
func runPrep(c *client, cfg smokeConfig, tag string) error {
	step("M3 shoot prep resources")
	assistant, err := c.createScheduleResource("assistant", "Smoke Assistant "+tag, 0, 0)
	if err != nil {
		return err
	}
	equipment, err := c.createScheduleResource("equipment", "Smoke Lens "+tag, 0, 0)
	if err != nil {
		return err
	}
	// Keep the shoot far enough out that the reminder fires after the
	// pre-shoot writes finish. Local runs without a lead skip the wait below.
	start := time.Now().UTC().Add(48 * time.Hour)
	if cfg.ReminderLead > 0 {
		start = time.Now().UTC().Add(cfg.ReminderLead + 20*time.Second)
	}
	start = start.Truncate(time.Second)
	end := start.Add(2 * time.Hour)
	fmt.Printf("  assistant=%s equipment=%s shoot=%s\n",
		assistant.ID, equipment.ID, start.Format(time.RFC3339))

	step("M3 prep booking")
	projectID, err := c.createSignedProject(cfg, tag, "prep", 50000)
	if err != nil {
		return err
	}
	order, err := c.createOrder(projectID, "deposit")
	if err != nil {
		return err
	}
	hold, err := c.createHold(projectID, []uuid.UUID{assistant.ID, equipment.ID}, start, end)
	if err != nil {
		return err
	}
	if hold.Status != "held" {
		return fmt.Errorf("prep hold = %+v", hold)
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/call-sheets",
		c.token, nil, 409, "illegal_state"); err != nil {
		return err
	}
	planned, err := c.listNotifications(notificationQuery(hold.ID, "hold_expiry", ""))
	if err != nil {
		return err
	}
	if len(planned.Items) == 0 {
		return fmt.Errorf("held booking %s planned no hold_expiry reminder", hold.ID)
	}
	paidEvent := map[string]any{
		"event_id": "smoke-prep-deposit-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": order.OutTradeNo,
		"provider_trade_no": "wx-prep-" + tag, "amount_cents": order.AmountCents, "currency": order.Currency,
		"paid_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectPaymentCallback(paidEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	booking, err := c.getBooking(hold.ID)
	if err != nil {
		return err
	}
	if booking.Status != "confirmed" {
		return fmt.Errorf("booking after deposit = %+v", booking)
	}
	pendingHold, err := c.listNotifications(notificationQuery(hold.ID, "hold_expiry", "pending"))
	if err != nil {
		return err
	}
	if len(pendingHold.Items) != 0 {
		return fmt.Errorf("confirm left %d pending hold_expiry reminders", len(pendingHold.Items))
	}
	fmt.Println("  freeze on held -> 409 illegal_state, deposit confirms and cancels the hold reminder")

	step("M3 shot list")
	list, err := c.createShotList(hold.ID, tag)
	if err != nil {
		return err
	}
	if list.Version != 1 || len(list.Items) != 0 {
		return fmt.Errorf("new shot list = %+v", list)
	}
	if err := c.expectHeaders(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/shot-list",
		c.token, map[string]string{"Idempotency-Key": "smoke-prep-duplist-" + tag},
		map[string]any{"title": "again"}, 409, "shot_list_exists"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/shot-lists/"+list.ID.String()+"/items", c.token,
		map[string]any{"title": "bad ref", "reference_url": "javascript:alert(1)"}, 422, "validation_failed"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/shot-lists/"+list.ID.String()+"/items", c.token,
		map[string]any{"title": "bad priority", "priority": "urgent"}, 422, "validation_failed"); err != nil {
		return err
	}
	scriptTitle := `<script>alert("x")</script>`
	first, err := c.addShotItem(list.ID, tag, map[string]any{
		"category": "portrait", "title": scriptTitle, "description": "hero frame",
		"reference_url": "https://example.com/ref.jpg", "priority": "high",
	})
	if err != nil {
		return err
	}
	second, err := c.addShotItem(list.ID, tag, map[string]any{
		"category": "detail", "title": "Ring detail", "priority": "normal",
	})
	if err != nil {
		return err
	}
	third, err := c.addShotItem(list.ID, tag, map[string]any{
		"category": "group", "title": "Family group", "priority": "low",
	})
	if err != nil {
		return err
	}
	if first.SortOrder != 0 || second.SortOrder != 1 || third.SortOrder != 2 || first.Priority != "high" {
		return fmt.Errorf("shot items = %+v / %+v / %+v", first, second, third)
	}
	if err := c.do(http.MethodPatch, "/api/v1/shot-items/"+first.ID.String(), c.token,
		map[string]any{"expected_version": first.Version, "done": true}, &first); err != nil {
		return err
	}
	if !first.Done || first.DoneAt == nil || first.Version != 2 {
		return fmt.Errorf("done shot item = %+v", first)
	}
	if err := c.expect(http.MethodPatch, "/api/v1/shot-items/"+first.ID.String(), c.token,
		map[string]any{"expected_version": 1, "title": "stale"}, 409, "version_conflict"); err != nil {
		return err
	}
	if err := c.deleteShotItem(third.ID, third.Version); err != nil {
		return err
	}
	var fetched prepShotList
	if err := c.do(http.MethodGet, "/api/v1/bookings/"+hold.ID.String()+"/shot-list", c.token, nil, &fetched); err != nil {
		return err
	}
	if len(fetched.Items) != 2 {
		return fmt.Errorf("shot list after delete has %d items, want 2", len(fetched.Items))
	}
	fmt.Printf("  list=%s items=%d (one done, one deleted), bad URL/priority/stale version rejected\n",
		fetched.ID, len(fetched.Items))

	step("M3 gear checklist")
	if err := c.expect(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/gear", c.token,
		map[string]any{"resource_id": equipment.ID, "quantity": 101}, 422, "validation_failed"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/gear", c.token,
		map[string]any{"resource_id": assistant.ID}, 422, "validation_failed"); err != nil {
		return err
	}
	gear, err := c.addGear(hold.ID, tag, map[string]any{
		"resource_id": equipment.ID, "category": "camera", "quantity": 2,
	})
	if err != nil {
		return err
	}
	if gear.Name != equipment.Name || gear.Quantity != 2 || gear.Checked || gear.ResourceName != equipment.Name {
		return fmt.Errorf("gear after add = %+v", gear)
	}
	if err := c.expectHeaders(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/gear",
		c.token, map[string]string{"Idempotency-Key": "smoke-prep-dupgear-" + tag},
		map[string]any{"resource_id": equipment.ID}, 409, "gear_conflict"); err != nil {
		return err
	}
	if err := c.do(http.MethodPatch, "/api/v1/gear/"+gear.ID.String(), c.token,
		map[string]any{"expected_version": gear.Version, "checked": true}, &gear); err != nil {
		return err
	}
	if !gear.Checked || gear.CheckedAt == nil || gear.Version != 2 {
		return fmt.Errorf("checked gear = %+v", gear)
	}
	fmt.Printf("  gear=%s resource=%s checked at %s\n", gear.Name, gear.ResourceName,
		gear.CheckedAt.Format(time.RFC3339))

	step("M3 authorizations")
	if err := c.expect(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/authorizations", c.token,
		map[string]any{"kind": "unknown", "subject": "x"}, 422, "validation_failed"); err != nil {
		return err
	}
	auth, err := c.createAuthorization(hold.ID, tag, "model_release", "Smoke Model "+tag)
	if err != nil {
		return err
	}
	if auth.Status != "pending" || auth.SignedAt != nil {
		return fmt.Errorf("new authorization = %+v", auth)
	}
	if err := c.expectHeaders(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/authorizations",
		c.token, map[string]string{"Idempotency-Key": "smoke-prep-dupauth-" + tag},
		map[string]any{"kind": "model_release", "subject": "Smoke Model " + tag}, 409, "authorization_conflict"); err != nil {
		return err
	}
	shootPlanned, err := c.listNotifications(notificationQuery(hold.ID, "shoot_reminder", ""))
	if err != nil {
		return err
	}
	if len(shootPlanned.Items) == 0 {
		return fmt.Errorf("confirmed booking planned no shoot_reminder")
	}
	authPlanned, err := c.listNotifications(notificationQuery(hold.ID, "auth_incomplete", ""))
	if err != nil {
		return err
	}
	if len(authPlanned.Items) == 0 {
		return fmt.Errorf("pending authorization did not plan an auth_incomplete reminder")
	}
	fmt.Println("  duplicate kind/subject -> 409, pending auth plans an auth_incomplete reminder")

	step("M3 call sheet v1")
	sheetV1, err := c.freezeCallSheet(hold.ID, tag, "v1")
	if err != nil {
		return err
	}
	if sheetV1.Version != 1 || len(sheetV1.SnapshotSHA256) != 64 {
		return fmt.Errorf("call sheet v1 = %+v", sheetV1)
	}
	localHash := sha256.Sum256(sheetV1.Snapshot)
	if got := hex.EncodeToString(localHash[:]); got != sheetV1.SnapshotSHA256 {
		return fmt.Errorf("snapshot hash mismatch: %s vs %s", got, sheetV1.SnapshotSHA256)
	}
	var snap prepSnapshot
	if err := json.Unmarshal(sheetV1.Snapshot, &snap); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	if snap.Schema != "call_sheet.v1" || snap.Booking.ID != hold.ID || !snap.Booking.StartsAt.Equal(start) {
		return fmt.Errorf("snapshot header = schema %q booking %s starts %s", snap.Schema, snap.Booking.ID, snap.Booking.StartsAt)
	}
	if snap.ShotList == nil || len(snap.ShotList.Items) != 2 ||
		snap.ShotList.Items[0].Title != scriptTitle || !snap.ShotList.Items[0].Done {
		return fmt.Errorf("snapshot shot list = %+v", snap.ShotList)
	}
	if len(snap.Gear) != 1 || !snap.Gear[0].Checked || snap.Gear[0].Quantity != 2 {
		return fmt.Errorf("snapshot gear = %+v", snap.Gear)
	}
	if len(snap.Authorizations) != 1 || snap.Authorizations[0].Status != "pending" {
		return fmt.Errorf("snapshot authorizations = %+v", snap.Authorizations)
	}
	if len(snap.Booking.Resources) != 2 {
		return fmt.Errorf("snapshot resources = %+v", snap.Booking.Resources)
	}
	if err := c.verifyCallSheetHTML(sheetV1, scriptTitle); err != nil {
		return err
	}
	fmt.Printf("  v1 sha256=%s shot_items=%d gear=%d auths=%d, HTML escapes and is locked down\n",
		sheetV1.SnapshotSHA256[:16]+"...", len(snap.ShotList.Items), len(snap.Gear), len(snap.Authorizations))

	step("M3 reminder delivery")
	shootNotice, authNotice, err := c.waitPrepReminders(cfg, hold.ID)
	if err != nil {
		return err
	}
	if shootNotice != nil {
		if authNotice == nil || authNotice.Type != "auth_incomplete" ||
			len(authNotice.Deliveries) == 0 || authNotice.Deliveries[0].Status != "sent" {
			return fmt.Errorf("auth_incomplete delivery = %+v", authNotice)
		}
		if c.alerts != nil {
			event, err := c.alerts.waitForReminder(shootNotice.ID, 20*time.Second)
			if err != nil {
				return err
			}
			if event.Event != "notification" || event.Type != "shoot_reminder" ||
				event.SubjectID != hold.ID || event.HeaderType != "shoot_reminder" {
				return fmt.Errorf("reminder webhook payload = %+v", event)
			}
			fmt.Printf("  signed reminder webhook accepted for %s\n", event.Type)
		}
		before, err := c.listNotifications(notificationQuery(hold.ID, "", ""))
		if err != nil {
			return err
		}
		var read noticeItem
		if err := c.do(http.MethodPost, "/api/v1/notifications/"+shootNotice.ID.String()+"/read",
			c.token, nil, &read); err != nil {
			return err
		}
		if read.ReadAt == nil {
			return fmt.Errorf("mark read left read_at empty: %+v", read)
		}
		after, err := c.listNotifications(notificationQuery(hold.ID, "", ""))
		if err != nil {
			return err
		}
		if after.Unread != before.Unread-1 {
			return fmt.Errorf("unread after mark read = %d, want %d", after.Unread, before.Unread-1)
		}
		fmt.Printf("  notifications read (unread %d -> %d), deliveries attempts=%d\n",
			before.Unread, after.Unread, shootNotice.Attempts)
	} else {
		fmt.Println("  delivery wait skipped (pass -reminder-lead to assert webhooks)")
	}

	step("M3 authorization signed")
	if err := c.do(http.MethodPatch, "/api/v1/authorizations/"+auth.ID.String(), c.token,
		map[string]any{"expected_version": auth.Version, "status": "signed"}, &auth); err != nil {
		return err
	}
	if auth.Status != "signed" || auth.SignedAt == nil || auth.Version != 2 {
		return fmt.Errorf("signed authorization = %+v", auth)
	}
	if err := c.expect(http.MethodDelete, "/api/v1/authorizations/"+auth.ID.String()+"?expected_version="+
		fmt.Sprint(auth.Version), c.token, nil, 409, "illegal_state"); err != nil {
		return err
	}
	pendingAuth, err := c.listNotifications(notificationQuery(hold.ID, "auth_incomplete", "pending"))
	if err != nil {
		return err
	}
	if len(pendingAuth.Items) != 0 {
		return fmt.Errorf("signed authorization left %d pending auth_incomplete reminders", len(pendingAuth.Items))
	}
	fmt.Println("  signed_at set by the server, signed evidence cannot be deleted, no pending auth reminder")

	step("M3 reschedule re-plans reminders")
	newStart, newEnd := start.Add(2*time.Hour), end.Add(2*time.Hour)
	moved, err := c.rescheduleBooking(hold.ID, booking.Version, newStart, newEnd, nil)
	if err != nil {
		return err
	}
	if moved.Status != "confirmed" || !moved.StartsAt.Equal(newStart) {
		return fmt.Errorf("rescheduled prep booking = %+v", moved)
	}
	pendingShoot, err := c.listNotifications(notificationQuery(hold.ID, "shoot_reminder", "pending"))
	if err != nil {
		return err
	}
	var replanned *noticeItem
	for i := range pendingShoot.Items {
		var payload struct {
			StartsAt *time.Time `json:"starts_at"`
		}
		if err := json.Unmarshal(pendingShoot.Items[i].Payload, &payload); err != nil {
			return fmt.Errorf("decode reminder payload: %w", err)
		}
		if payload.StartsAt != nil && payload.StartsAt.Equal(newStart) {
			replanned = &pendingShoot.Items[i]
			break
		}
	}
	if replanned == nil {
		return fmt.Errorf("reschedule did not re-plan shoot reminders for %s: %+v", newStart, pendingShoot.Items)
	}
	if !replanned.DueAt.Before(newStart) {
		return fmt.Errorf("replanned reminder due %s, want before %s", replanned.DueAt, newStart)
	}

	step("M3 call sheet v2")
	sheetV2, err := c.freezeCallSheet(hold.ID, tag, "v2")
	if err != nil {
		return err
	}
	if sheetV2.Version != 2 || sheetV2.SnapshotSHA256 == sheetV1.SnapshotSHA256 {
		return fmt.Errorf("call sheet v2 = %+v (v1 hash %s)", sheetV2, sheetV1.SnapshotSHA256)
	}
	var fetchedV1 prepCallSheet
	if err := c.do(http.MethodGet, "/api/v1/call-sheets/"+sheetV1.ID.String(), c.token, nil, &fetchedV1); err != nil {
		return err
	}
	if fetchedV1.Version != 1 || fetchedV1.SnapshotSHA256 != sheetV1.SnapshotSHA256 {
		return fmt.Errorf("call sheet v1 changed after v2: %+v", fetchedV1)
	}
	var snapV1 prepSnapshot
	if err := json.Unmarshal(fetchedV1.Snapshot, &snapV1); err != nil {
		return err
	}
	if !snapV1.Booking.StartsAt.Equal(start) {
		return fmt.Errorf("call sheet v1 starts_at = %s, want %s", snapV1.Booking.StartsAt, start)
	}
	var snapV2 prepSnapshot
	if err := json.Unmarshal(sheetV2.Snapshot, &snapV2); err != nil {
		return err
	}
	if !snapV2.Booking.StartsAt.Equal(newStart) {
		return fmt.Errorf("call sheet v2 starts_at = %s, want %s", snapV2.Booking.StartsAt, newStart)
	}
	fmt.Printf("  v2 sha256=%s (differs from v1), v1 still %s\n",
		sheetV2.SnapshotSHA256[:16]+"...", sheetV1.SnapshotSHA256[:16]+"...")

	step("M3 cancel keeps prep history")
	cancelled, err := c.cancelBooking(hold.ID, moved.Version, "prep cancel")
	if err != nil {
		return err
	}
	if cancelled.Status != "cancelled" {
		return fmt.Errorf("cancelled prep booking = %+v", cancelled)
	}
	pendingAny, err := c.listNotifications(notificationQuery(hold.ID, "", "pending"))
	if err != nil {
		return err
	}
	if len(pendingAny.Items) != 0 {
		return fmt.Errorf("cancel left %d pending reminders", len(pendingAny.Items))
	}
	var sheets struct {
		Total int                    `json:"total"`
		Items []prepCallSheetSummary `json:"items"`
	}
	if err := c.do(http.MethodGet, "/api/v1/bookings/"+hold.ID.String()+"/call-sheets",
		c.token, nil, &sheets); err != nil {
		return err
	}
	if sheets.Total != 2 {
		return fmt.Errorf("call sheets after cancel = %d, want 2", sheets.Total)
	}
	for _, sheet := range []*prepCallSheet{sheetV1, sheetV2} {
		var fetched prepCallSheet
		if err := c.do(http.MethodGet, "/api/v1/call-sheets/"+sheet.ID.String(), c.token, nil, &fetched); err != nil {
			return err
		}
		if fetched.SnapshotSHA256 != sheet.SnapshotSHA256 {
			return fmt.Errorf("call sheet v%d changed after cancel", sheet.Version)
		}
	}
	if err := c.expect(http.MethodPost, "/api/v1/shot-lists/"+list.ID.String()+"/items", c.token,
		map[string]any{"title": "too late"}, 409, "illegal_state"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/gear", c.token,
		map[string]any{"name": "late tripod"}, 409, "illegal_state"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPatch, "/api/v1/authorizations/"+auth.ID.String(), c.token,
		map[string]any{"expected_version": auth.Version, "status": "signed"}, 409, "illegal_state"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/call-sheets",
		c.token, nil, 409, "illegal_state"); err != nil {
		return err
	}
	fmt.Println("  pending reminders cancelled, v1/v2 readable, prep writes -> 409 illegal_state")
	return nil
}

// waitPrepReminders waits for scheduled deliveries. Local runs without a
// reminder lead skip the wait and only rely on the planning assertions.
func (c *client) waitPrepReminders(cfg smokeConfig, bookingID uuid.UUID) (*noticeItem, *noticeItem, error) {
	if cfg.ReminderLead <= 0 {
		return nil, nil, nil
	}
	shoot, err := c.waitNotification(bookingID, "shoot_reminder", "sent", 60*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if shoot.Attempts < 1 || len(shoot.Deliveries) == 0 ||
		shoot.Deliveries[0].Status != "sent" || shoot.Deliveries[0].Attempts < 1 {
		return nil, nil, fmt.Errorf("shoot reminder deliveries = %+v", shoot.Deliveries)
	}
	auth, err := c.waitNotification(bookingID, "auth_incomplete", "sent", 60*time.Second)
	if err != nil {
		return nil, nil, err
	}
	return shoot, auth, nil
}

func (c *client) waitNotification(bookingID uuid.UUID, typ, want string, timeout time.Duration) (*noticeItem, error) {
	deadline := time.Now().Add(timeout)
	var last []noticeItem
	for {
		list, err := c.listNotifications(notificationQuery(bookingID, typ, ""))
		if err != nil {
			return nil, err
		}
		last = list.Items
		for i := range list.Items {
			if list.Items[i].Status != want {
				continue
			}
			var detail noticeItem
			if err := c.do(http.MethodGet, "/api/v1/notifications/"+list.Items[i].ID.String(),
				c.token, nil, &detail); err != nil {
				return nil, err
			}
			return &detail, nil
		}
		if time.Now().After(deadline) {
			states := make([]string, 0, len(last))
			for _, n := range last {
				states = append(states, n.Status)
			}
			return nil, fmt.Errorf("%s reminder for %s is %v after %s, want %s (is the worker running?)",
				typ, bookingID, states, timeout, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func notificationQuery(bookingID uuid.UUID, typ, status string) url.Values {
	q := url.Values{}
	if bookingID != uuid.Nil {
		q.Set("booking_id", bookingID.String())
	}
	if typ != "" {
		q.Set("type", typ)
	}
	if status != "" {
		q.Set("status", status)
	}
	q.Set("limit", "100")
	return q
}

func (c *client) listNotifications(query url.Values) (*noticeList, error) {
	var list noticeList
	if err := c.do(http.MethodGet, "/api/v1/notifications?"+query.Encode(), c.token, nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *client) createShotList(bookingID uuid.UUID, tag string) (*prepShotList, error) {
	var list prepShotList
	headers := map[string]string{"Idempotency-Key": "smoke-prep-shotlist-" + tag}
	if err := c.doHeaders(http.MethodPost, "/api/v1/bookings/"+bookingID.String()+"/shot-list",
		c.token, headers, map[string]any{
			"title": "Smoke shot list", "note": "prep smoke",
		}, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *client) addShotItem(listID uuid.UUID, tag string, body map[string]any) (*prepShotItem, error) {
	var item prepShotItem
	headers := map[string]string{
		"Idempotency-Key": "smoke-prep-shotitem-" + tag + "-" + shortRandom(),
	}
	if err := c.doHeaders(http.MethodPost, "/api/v1/shot-lists/"+listID.String()+"/items",
		c.token, headers, body, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (c *client) deleteShotItem(itemID uuid.UUID, version int) error {
	status, raw, err := c.doStatus(http.MethodDelete,
		"/api/v1/shot-items/"+itemID.String()+"?expected_version="+fmt.Sprint(version), c.token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("delete shot item: %d: %s", status, string(raw))
	}
	return nil
}

func (c *client) addGear(bookingID uuid.UUID, tag string, body map[string]any) (*prepGear, error) {
	var item prepGear
	headers := map[string]string{"Idempotency-Key": "smoke-prep-gear-" + tag}
	if err := c.doHeaders(http.MethodPost, "/api/v1/bookings/"+bookingID.String()+"/gear",
		c.token, headers, body, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (c *client) createAuthorization(bookingID uuid.UUID, tag, kind, subject string) (*prepAuthorization, error) {
	var a prepAuthorization
	headers := map[string]string{"Idempotency-Key": "smoke-prep-auth-" + tag}
	if err := c.doHeaders(http.MethodPost, "/api/v1/bookings/"+bookingID.String()+"/authorizations",
		c.token, headers, map[string]any{"kind": kind, "subject": subject}, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (c *client) freezeCallSheet(bookingID uuid.UUID, tag, version string) (*prepCallSheet, error) {
	var sheet prepCallSheet
	headers := map[string]string{"Idempotency-Key": "smoke-prep-sheet-" + tag + "-" + version}
	if err := c.doHeaders(http.MethodPost, "/api/v1/bookings/"+bookingID.String()+"/call-sheets",
		c.token, headers, nil, &sheet); err != nil {
		return nil, err
	}
	return &sheet, nil
}

func (c *client) verifyCallSheetHTML(sheet *prepCallSheet, scriptTitle string) error {
	status, headers, page, err := c.getRaw("/api/v1/call-sheets/" + sheet.ID.String() + "/html")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("call sheet HTML: %d: %s", status, string(page))
	}
	if ct := headers.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		return fmt.Errorf("call sheet HTML content-type = %q", ct)
	}
	if headers.Get("Cache-Control") != "no-store" ||
		headers.Get("X-Content-Type-Options") != "nosniff" {
		return fmt.Errorf("call sheet HTML security headers missing: %v", headers)
	}
	csp := headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "form-action 'none'") {
		return fmt.Errorf("call sheet CSP = %q", csp)
	}
	body := string(page)
	if !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, scriptTitle) {
		return fmt.Errorf("call sheet HTML did not escape the script title")
	}
	if strings.Contains(body, "<script") {
		return fmt.Errorf("call sheet HTML contains a script tag")
	}
	if !strings.Contains(body, sheet.SnapshotSHA256) || !strings.Contains(body, "v1") {
		return fmt.Errorf("call sheet HTML footer lacks version/hash")
	}
	return nil
}

func (c *client) getRaw(path string) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, raw, nil
}
