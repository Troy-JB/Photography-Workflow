package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/payment"
)

type schedRule struct {
	Weekday     int `json:"weekday"`
	StartMinute int `json:"start_minute"`
	EndMinute   int `json:"end_minute"`
}

type schedResource struct {
	ID      uuid.UUID   `json:"id"`
	Kind    string      `json:"kind"`
	Name    string      `json:"name"`
	Active  bool        `json:"active"`
	Version int         `json:"version"`
	Rules   []schedRule `json:"rules"`
}

type schedBusy struct {
	StartsAt  time.Time  `json:"starts_at"`
	EndsAt    time.Time  `json:"ends_at"`
	Kind      string     `json:"kind"`
	BookingID *uuid.UUID `json:"booking_id"`
}

type schedAvailability struct {
	Resources []struct {
		ResourceID uuid.UUID   `json:"resource_id"`
		Name       string      `json:"name"`
		Busy       []schedBusy `json:"busy"`
	} `json:"resources"`
}

type schedBooking struct {
	ID            uuid.UUID  `json:"id"`
	ProjectID     uuid.UUID  `json:"project_id"`
	Status        string     `json:"status"`
	StartsAt      time.Time  `json:"starts_at"`
	EndsAt        time.Time  `json:"ends_at"`
	HoldExpiresAt *time.Time `json:"hold_expires_at"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`
	Version       int        `json:"version"`
}

type schedBlock struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

type schedProject struct {
	ID           uuid.UUID  `json:"id"`
	State        string     `json:"state"`
	ShootStartAt *time.Time `json:"shoot_start_at"`
	ShootEndAt   *time.Time `json:"shoot_end_at"`
}

// runScheduling exercises the M3 loop: resources and weekly rules, a hold with
// TTL, the exclusion constraint as the sole double-booking arbiter, deposit
// settlement auto-confirmation, reschedule/cancel, worker expiry and the
// concurrent contention check.
func runScheduling(c *client, cfg smokeConfig, tag string) error {
	loc, err := time.LoadLocation(cfg.ScheduleTimezone)
	if err != nil {
		return fmt.Errorf("load schedule timezone %q: %w", cfg.ScheduleTimezone, err)
	}

	step("M3 resources and availability rules")
	photo, err := c.createScheduleResource("photographer", "Smoke Photographer "+tag, 30, 15)
	if err != nil {
		return err
	}
	studio, err := c.createScheduleResource("studio", "Smoke Studio "+tag, 0, 0)
	if err != nil {
		return err
	}
	daily := make([]schedRule, 0, 7)
	for weekday := 1; weekday <= 7; weekday++ {
		daily = append(daily, schedRule{Weekday: weekday, StartMinute: 9 * 60, EndMinute: 18 * 60})
	}
	if _, err := c.setAvailability(photo, daily); err != nil {
		return err
	}
	if _, err := c.setAvailability(studio, daily); err != nil {
		return err
	}
	fmt.Printf("  photographer=%s studio=%s 09:00-18:00 daily\n", photo.ID, studio.ID)

	// Every slot lives two weeks out so the run never collides with real data.
	base := time.Now().In(loc).AddDate(0, 0, 14)
	day := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, loc)
	at := func(hour, minute int) time.Time {
		return day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	slotStart, slotEnd := at(10, 0), at(12, 0)

	step("M3 availability query")
	initial, err := c.scheduleAvailability(slotStart, slotEnd, "")
	if err != nil {
		return err
	}
	if len(initial.Resources) < 2 {
		return fmt.Errorf("availability returned %d resources, want at least 2", len(initial.Resources))
	}
	if busy := busyFor(initial, photo.ID); len(busy) != 0 {
		return fmt.Errorf("fresh photographer already busy: %+v", busy)
	}
	fmt.Printf("  %d resources visible, slot is free\n", len(initial.Resources))

	step("M3 signed order and hold")
	projectID, err := c.createSignedProject(cfg, tag, "hold", 50000)
	if err != nil {
		return err
	}
	// Create the order before the hold so the callback can settle well inside
	// even a five-second CI hold TTL.
	order, err := c.createOrder(projectID, "deposit")
	if err != nil {
		return err
	}
	hold, err := c.createHold(projectID, []uuid.UUID{photo.ID, studio.ID}, slotStart, slotEnd)
	if err != nil {
		return err
	}
	if hold.Status != "held" || hold.HoldExpiresAt == nil {
		return fmt.Errorf("hold = %+v", hold)
	}
	fmt.Printf("  booking=%s held until %s\n", hold.ID, hold.HoldExpiresAt.In(loc).Format("15:04:05"))

	step("M3 deposit auto-confirms the hold")
	paidEvent := map[string]any{
		"event_id": "smoke-sched-deposit-" + tag, "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": order.OutTradeNo,
		"provider_trade_no": "wx-sched-" + tag, "amount_cents": order.AmountCents, "currency": order.Currency,
		"paid_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectPaymentCallback(paidEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	confirmed, err := c.getBooking(hold.ID)
	if err != nil {
		return err
	}
	if confirmed.Status != "confirmed" || confirmed.ConfirmedAt == nil || confirmed.HoldExpiresAt != nil {
		return fmt.Errorf("booking after deposit = %+v", confirmed)
	}
	project, err := c.getSchedProject(projectID)
	if err != nil {
		return err
	}
	if project.ShootStartAt == nil || !project.ShootStartAt.Equal(slotStart) ||
		project.ShootEndAt == nil || !project.ShootEndAt.Equal(slotEnd) {
		return fmt.Errorf("project shoot times after confirm = %+v", project)
	}
	again, err := c.confirmBooking(hold.ID, confirmed.Version)
	if err != nil {
		return err
	}
	if again.Status != "confirmed" || again.Version != confirmed.Version {
		return fmt.Errorf("repeated confirm = %+v", again)
	}
	confirmed = again
	fmt.Println("  deposit callback -> confirmed, project shoot times synced, repeated confirm is a no-op")

	step("M3 double-booking is rejected")
	otherID, err := c.createDraftProject(tag, "conflict")
	if err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings", c.token, map[string]any{
		"project_id": otherID, "resource_ids": []uuid.UUID{photo.ID, studio.ID},
		"starts_at": slotStart, "ends_at": slotEnd,
	}, 409, "slot_taken"); err != nil {
		return err
	}
	block, err := c.createBlock(photo.ID, "manual", at(14, 0), at(15, 0), "smoke maintenance")
	if err != nil {
		return err
	}
	if block.Kind != "manual" {
		return fmt.Errorf("manual block = %+v", block)
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings", c.token, map[string]any{
		"project_id": otherID, "resource_ids": []uuid.UUID{photo.ID},
		"starts_at": at(13, 30), "ends_at": at(14, 30),
	}, 409, "slot_taken"); err != nil {
		return err
	}
	fmt.Println("  overlapping hold -> 409 slot_taken, manual block -> 409 slot_taken")

	step("M3 availability is enforced")
	outsideID, err := c.createDraftProject(tag, "outside")
	if err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings", c.token, map[string]any{
		"project_id": outsideID, "resource_ids": []uuid.UUID{photo.ID},
		"starts_at": at(8, 0), "ends_at": at(9, 0),
	}, 422, "outside_availability"); err != nil {
		return err
	}
	if err := c.deleteBlock(photo.ID, block.ID); err != nil {
		return err
	}
	fmt.Println("  08:00 booking on a 09:00-18:00 resource -> 422 outside_availability, block deleted")

	step("M3 reschedule")
	newStart, newEnd := at(13, 0), at(15, 0)
	moved, err := c.rescheduleBooking(hold.ID, confirmed.Version, newStart, newEnd, nil)
	if err != nil {
		return err
	}
	if moved.Status != "confirmed" || !moved.StartsAt.Equal(newStart) || moved.Version != confirmed.Version+1 {
		return fmt.Errorf("rescheduled booking = %+v", moved)
	}
	project, err = c.getSchedProject(projectID)
	if err != nil {
		return err
	}
	if project.ShootStartAt == nil || !project.ShootStartAt.Equal(newStart) ||
		project.ShootEndAt == nil || !project.ShootEndAt.Equal(newEnd) {
		return fmt.Errorf("project shoot times after reschedule = %+v", project)
	}
	oldWindow, err := c.scheduleAvailability(slotStart, slotEnd, "")
	if err != nil {
		return err
	}
	if busy := busyFor(oldWindow, photo.ID); len(busy) != 0 {
		return fmt.Errorf("old slot still busy after reschedule: %+v", busy)
	}
	newWindow, err := c.scheduleAvailability(newStart, newEnd, "")
	if err != nil {
		return err
	}
	if busy := busyFor(newWindow, photo.ID); len(busy) == 0 {
		return fmt.Errorf("new slot is not marked busy after reschedule")
	}
	fmt.Println("  old slot released, new slot occupied, project shoot times updated")

	step("M3 cancel")
	cancelled, err := c.cancelBooking(hold.ID, moved.Version, "smoke cancel")
	if err != nil {
		return err
	}
	if cancelled.Status != "cancelled" {
		return fmt.Errorf("cancelled booking = %+v", cancelled)
	}
	project, err = c.getSchedProject(projectID)
	if err != nil {
		return err
	}
	if project.State != "cancelled" || project.ShootStartAt != nil || project.ShootEndAt != nil {
		return fmt.Errorf("cancel did not cancel project or clear shoot times: %+v", project)
	}
	var selectableProjects struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
	}
	if err := c.do(http.MethodGet, "/api/v1/projects?limit=200&exclude_closed=true", c.token, nil, &selectableProjects); err != nil {
		return err
	}
	for _, item := range selectableProjects.Items {
		if item.ID == projectID {
			return fmt.Errorf("cancelled project %s remains in new-work project selector", projectID)
		}
	}
	if err := c.expect(http.MethodPost, "/api/v1/bookings/"+hold.ID.String()+"/cancel", c.token,
		map[string]any{"expected_version": cancelled.Version, "reason": "again"}, 409, ""); err != nil {
		return err
	}
	afterCancel, err := c.scheduleAvailability(newStart, newEnd, "")
	if err != nil {
		return err
	}
	if busy := busyFor(afterCancel, photo.ID); len(busy) != 0 {
		return fmt.Errorf("cancel left the slot busy: %+v", busy)
	}
	fmt.Println("  confirmed -> cancelled, project cancelled and shoot times cleared, repeat cancel -> 409")

	if cfg.ScheduleHoldTTL <= 30*time.Second {
		if err := runHoldExpiry(c, cfg, tag, photo.ID); err != nil {
			return err
		}
	} else {
		step("M3 hold expiry")
		fmt.Printf("  skipped (hold_ttl=%s; run with a TTL of 30s or less to exercise the worker sweep)\n",
			cfg.ScheduleHoldTTL)
	}

	if cfg.BookingRace > 0 {
		if err := runBookingRace(c, cfg, tag); err != nil {
			return err
		}
	}
	return nil
}

func runHoldExpiry(c *client, cfg smokeConfig, tag string, resourceID uuid.UUID) error {
	step("M3 hold expiry")
	projectID, err := c.createDraftProject(tag, "expiry")
	if err != nil {
		return err
	}
	start := time.Now().UTC().AddDate(0, 0, 21)
	start = time.Date(start.Year(), start.Month(), start.Day(), 2, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	hold, err := c.createHold(projectID, []uuid.UUID{resourceID}, start, end)
	if err != nil {
		return err
	}
	planned, err := c.listNotifications(notificationQuery(hold.ID, "hold_expiry", ""))
	if err != nil {
		return err
	}
	if len(planned.Items) == 0 {
		return fmt.Errorf("hold %s planned no hold_expiry reminder", hold.ID)
	}
	reminder := planned.Items[0]
	if reminder.Status != "pending" || hold.HoldExpiresAt == nil || reminder.DueAt.After(*hold.HoldExpiresAt) {
		return fmt.Errorf("hold_expiry reminder = %+v, want pending and due no later than %v",
			reminder, hold.HoldExpiresAt)
	}
	fmt.Printf("  hold_expiry reminder planned, due %s\n", reminder.DueAt.Format(time.RFC3339))
	deadline := time.Now().Add(cfg.ScheduleHoldTTL + 30*time.Second)
	for {
		current, gerr := c.getBooking(hold.ID)
		if gerr != nil {
			return gerr
		}
		if current.Status == "expired" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hold %s still %s after %s (is the worker running?)",
				hold.ID, current.Status, cfg.ScheduleHoldTTL+30*time.Second)
		}
		time.Sleep(250 * time.Millisecond)
	}
	nextID, err := c.createDraftProject(tag, "expiry-next")
	if err != nil {
		return err
	}
	reused, err := c.createHold(nextID, []uuid.UUID{resourceID}, start, end)
	if err != nil {
		return fmt.Errorf("slot was not released after expiry: %w", err)
	}
	if _, err := c.cancelBooking(reused.ID, reused.Version, "release after expiry check"); err != nil {
		return err
	}
	fmt.Printf("  hold expired by the worker after %s, slot reused and released\n", cfg.ScheduleHoldTTL)
	return nil
}

func runBookingRace(c *client, cfg smokeConfig, tag string) error {
	step(fmt.Sprintf("M3 concurrent booking race (%d)", cfg.BookingRace))
	equipment, err := c.createScheduleResource("equipment", "Smoke Camera "+tag, 0, 0)
	if err != nil {
		return err
	}
	start := time.Now().UTC().AddDate(0, 0, 28)
	start = time.Date(start.Year(), start.Month(), start.Day(), 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	projects := make([]uuid.UUID, cfg.BookingRace)
	for i := range projects {
		id, err := c.createDraftProject(tag, fmt.Sprintf("race-%d", i))
		if err != nil {
			return err
		}
		projects[i] = id
	}

	type raceResult struct {
		status int
		code   string
		body   []byte
		err    error
	}
	results := make([]raceResult, len(projects))
	var wg sync.WaitGroup
	for i, projectID := range projects {
		wg.Add(1)
		go func(i int, projectID uuid.UUID) {
			defer wg.Done()
			status, raw, err := c.doStatus(http.MethodPost, "/api/v1/bookings", c.token, map[string]any{
				"project_id": projectID, "resource_ids": []uuid.UUID{equipment.ID},
				"starts_at": start, "ends_at": end,
			})
			res := raceResult{status: status, body: raw, err: err}
			if err == nil && status != http.StatusCreated {
				var p struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal(raw, &p)
				res.code = p.Code
			}
			results[i] = res
		}(i, projectID)
	}
	wg.Wait()

	var winners []schedBooking
	conflicts := 0
	for i, res := range results {
		if res.err != nil {
			return fmt.Errorf("race request %d: %w", i, res.err)
		}
		switch {
		case res.status == http.StatusCreated:
			var b schedBooking
			if err := json.Unmarshal(res.body, &b); err != nil {
				return fmt.Errorf("decode race winner: %w", err)
			}
			winners = append(winners, b)
		case res.status == http.StatusConflict && res.code == "slot_taken":
			conflicts++
		default:
			return fmt.Errorf("race request %d: status %d code %q: %s", i, res.status, res.code, string(res.body))
		}
	}
	if len(winners) != 1 || conflicts != len(projects)-1 {
		return fmt.Errorf("race produced %d winners and %d slot_taken, want 1 and %d",
			len(winners), conflicts, len(projects)-1)
	}
	winner := winners[0]
	if _, err := c.cancelBooking(winner.ID, winner.Version, "race cleanup"); err != nil {
		return err
	}
	fmt.Printf("  1 of %d holds succeeded, %d -> 409 slot_taken, winner cancelled\n", len(projects), conflicts)
	return nil
}

func (c *client) createScheduleResource(kind, name string, bufferBefore, bufferAfter int) (*schedResource, error) {
	var res schedResource
	if err := c.do(http.MethodPost, "/api/v1/resources", c.token, map[string]any{
		"kind": kind, "name": name,
		"buffer_before_minutes": bufferBefore, "buffer_after_minutes": bufferAfter,
	}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *client) setAvailability(res *schedResource, rules []schedRule) (*schedResource, error) {
	var out struct {
		Version int         `json:"version"`
		Rules   []schedRule `json:"rules"`
	}
	if err := c.do(http.MethodPut, "/api/v1/resources/"+res.ID.String()+"/availability", c.token, map[string]any{
		"expected_version": res.Version, "rules": rules,
	}, &out); err != nil {
		return nil, err
	}
	if out.Version != res.Version+1 || len(out.Rules) != len(rules) {
		return nil, fmt.Errorf("availability update = %+v", out)
	}
	res.Version = out.Version
	res.Rules = out.Rules
	return res, nil
}

func (c *client) scheduleAvailability(from, to time.Time, kind string) (*schedAvailability, error) {
	query := url.Values{}
	query.Set("from", from.Format(time.RFC3339))
	query.Set("to", to.Format(time.RFC3339))
	if kind != "" {
		query.Set("kind", kind)
	}
	var out schedAvailability
	if err := c.do(http.MethodGet, "/api/v1/schedule/availability?"+query.Encode(), c.token, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func busyFor(a *schedAvailability, resourceID uuid.UUID) []schedBusy {
	for _, res := range a.Resources {
		if res.ResourceID == resourceID {
			return res.Busy
		}
	}
	return nil
}

func (c *client) createDraftProject(tag, suffix string) (uuid.UUID, error) {
	var project struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects", c.token, map[string]any{
		"title": "Smoke Schedule " + suffix + " " + tag,
	}, &project); err != nil {
		return uuid.Nil, err
	}
	return project.ID, nil
}

// createSignedProject walks the minimum commerce path a booking confirmation
// needs: a deposit-bearing quote, a rendered contract and its signed callback.
func (c *client) createSignedProject(cfg smokeConfig, tag, suffix string, depositCents int64) (uuid.UUID, error) {
	var project struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects", c.token, map[string]any{
		"code": "SCH-" + tag + "-" + suffix, "title": "Smoke Schedule " + suffix + " " + tag,
		"total_amount_cents": depositCents * 2, "deposit_cents": depositCents, "balance_cents": depositCents,
	}, &project); err != nil {
		return uuid.Nil, err
	}
	var quote commerceQuote
	if err := c.do(http.MethodPost, "/api/v1/quotes", c.token, map[string]any{
		"project_id": project.ID, "deposit_cents": depositCents,
		"items": []map[string]any{
			{"kind": "service", "description": "Smoke schedule shoot", "quantity": 1,
				"unit_price_cents": depositCents * 2},
		},
	}, &quote); err != nil {
		return uuid.Nil, err
	}
	var sent commerceQuote
	if err := c.do(http.MethodPost, "/api/v1/quotes/"+quote.ID.String()+"/send", c.token,
		map[string]any{"expected_version": quote.RowVersion}, &sent); err != nil {
		return uuid.Nil, err
	}
	var contract commerceContract
	if err := c.do(http.MethodPost, "/api/v1/contracts", c.token, map[string]any{
		"project_id": project.ID, "quote_id": sent.ID, "signer_name": "Smoke Schedule " + tag,
	}, &contract); err != nil {
		return uuid.Nil, err
	}
	var signing commerceContract
	if err := c.do(http.MethodPost, "/api/v1/contracts/"+contract.ID.String()+"/send", c.token,
		map[string]any{"expected_version": contract.Version}, &signing); err != nil {
		return uuid.Nil, err
	}
	if err := c.expectCallback(map[string]any{
		"event_id":     "smoke-sched-contract-signing-" + tag + "-" + suffix,
		"sign_task_id": signing.SignTaskID, "type": "signing",
	}, cfg.ESignSecret, 200, "processed"); err != nil {
		return uuid.Nil, err
	}
	if err := c.expectCallback(map[string]any{
		"event_id":     "smoke-sched-contract-signed-" + tag + "-" + suffix,
		"sign_task_id": signing.SignTaskID, "type": "signed",
		"signer_name": "Smoke Schedule " + tag, "document_sha256": fmt.Sprintf("%064x", time.Now().UnixNano()),
		"signed_at": time.Now().UTC().Format(time.RFC3339),
	}, cfg.ESignSecret, 200, "processed"); err != nil {
		return uuid.Nil, err
	}
	return project.ID, nil
}

func (c *client) createHold(projectID uuid.UUID, resourceIDs []uuid.UUID, start, end time.Time) (*schedBooking, error) {
	var booking schedBooking
	if err := c.do(http.MethodPost, "/api/v1/bookings", c.token, map[string]any{
		"project_id": projectID, "resource_ids": resourceIDs, "starts_at": start, "ends_at": end,
	}, &booking); err != nil {
		return nil, err
	}
	return &booking, nil
}

func (c *client) getBooking(id uuid.UUID) (*schedBooking, error) {
	var booking schedBooking
	if err := c.do(http.MethodGet, "/api/v1/bookings/"+id.String(), c.token, nil, &booking); err != nil {
		return nil, err
	}
	return &booking, nil
}

func (c *client) confirmBooking(id uuid.UUID, version int) (*schedBooking, error) {
	var booking schedBooking
	if err := c.do(http.MethodPost, "/api/v1/bookings/"+id.String()+"/confirm", c.token,
		map[string]any{"expected_version": version}, &booking); err != nil {
		return nil, err
	}
	return &booking, nil
}

func (c *client) rescheduleBooking(id uuid.UUID, version int, start, end time.Time, resources []uuid.UUID) (*schedBooking, error) {
	body := map[string]any{"expected_version": version, "starts_at": start, "ends_at": end}
	if len(resources) > 0 {
		body["resource_ids"] = resources
	}
	var booking schedBooking
	if err := c.do(http.MethodPost, "/api/v1/bookings/"+id.String()+"/reschedule", c.token, body, &booking); err != nil {
		return nil, err
	}
	return &booking, nil
}

func (c *client) cancelBooking(id uuid.UUID, version int, reason string) (*schedBooking, error) {
	var booking schedBooking
	if err := c.do(http.MethodPost, "/api/v1/bookings/"+id.String()+"/cancel", c.token,
		map[string]any{"expected_version": version, "reason": reason}, &booking); err != nil {
		return nil, err
	}
	return &booking, nil
}

func (c *client) createBlock(resourceID uuid.UUID, kind string, start, end time.Time, note string) (*schedBlock, error) {
	var block schedBlock
	if err := c.do(http.MethodPost, "/api/v1/resources/"+resourceID.String()+"/blocks", c.token, map[string]any{
		"kind": kind, "starts_at": start, "ends_at": end, "note": note,
	}, &block); err != nil {
		return nil, err
	}
	return &block, nil
}

func (c *client) deleteBlock(resourceID, blockID uuid.UUID) error {
	status, raw, err := c.doStatus(http.MethodDelete,
		"/api/v1/resources/"+resourceID.String()+"/blocks/"+blockID.String(), c.token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("delete block: %d: %s", status, string(raw))
	}
	return nil
}

func (c *client) getSchedProject(projectID uuid.UUID) (*schedProject, error) {
	var project schedProject
	if err := c.do(http.MethodGet, "/api/v1/projects/"+projectID.String(), c.token, nil, &project); err != nil {
		return nil, err
	}
	return &project, nil
}
