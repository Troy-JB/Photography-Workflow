package schedule

import (
	"testing"
	"time"
)

func TestBookingStateMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     BookingStatus
		expired    bool
		confirm    bool
		reschedule bool
		cancel     bool
	}{
		{"held", StatusHeld, false, true, true, true},
		{"held but lapsed", StatusHeld, true, false, true, true},
		{"confirmed", StatusConfirmed, false, false, true, true},
		{"cancelled", StatusCancelled, false, false, false, false},
		{"expired", StatusExpired, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanConfirm(tc.status, tc.expired); got != tc.confirm {
				t.Fatalf("CanConfirm(%s, %v) = %v, want %v", tc.status, tc.expired, got, tc.confirm)
			}
			if got := CanReschedule(tc.status); got != tc.reschedule {
				t.Fatalf("CanReschedule(%s) = %v, want %v", tc.status, got, tc.reschedule)
			}
			if got := CanCancel(tc.status); got != tc.cancel {
				t.Fatalf("CanCancel(%s) = %v, want %v", tc.status, got, tc.cancel)
			}
		})
	}
}

func TestActiveStatus(t *testing.T) {
	for _, tc := range []struct {
		status BookingStatus
		want   bool
	}{
		{StatusHeld, true}, {StatusConfirmed, true},
		{StatusCancelled, false}, {StatusExpired, false},
	} {
		if got := IsActive(tc.status); got != tc.want {
			t.Fatalf("IsActive(%s) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestEnumValidation(t *testing.T) {
	for _, kind := range []string{KindPhotographer, KindStudio, KindMakeup, KindAssistant, KindEquipment} {
		if !ValidResourceKind(kind) {
			t.Fatalf("ValidResourceKind(%s) = false", kind)
		}
	}
	for _, kind := range []string{"", "lens", "PHOTOGRAPHER"} {
		if ValidResourceKind(kind) {
			t.Fatalf("ValidResourceKind(%q) = true", kind)
		}
	}
	if ValidManualBlockKind(BlockHold) || ValidManualBlockKind(BlockBooking) {
		t.Fatal("bookings must not create manual or closure blocks through the block endpoint")
	}
	if !ValidManualBlockKind(BlockManual) || !ValidManualBlockKind(BlockClosure) {
		t.Fatal("manual and closure must be accepted block kinds")
	}
	for _, status := range []string{"held", "confirmed", "cancelled", "expired"} {
		if !ValidBookingStatus(status) {
			t.Fatalf("ValidBookingStatus(%s) = false", status)
		}
	}
	if ValidBookingStatus("draft") {
		t.Fatal("draft is not a booking status")
	}
}

func TestWithinAvailability(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	day := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, loc) }

	daily := []AvailabilityRule{{Weekday: 1, StartMinute: 540, EndMinute: 1080}} // Mon 09:00-18:00

	// 2026-11-09 is a Monday.
	if !WithinAvailability(nil, day(9, 0, 0), day(10, 0, 0), loc) {
		t.Fatal("no rules must mean 24/7 availability")
	}
	if !WithinAvailability(daily, day(9, 9, 0), day(9, 10, 0), loc) {
		t.Fatal("booking inside the window was rejected")
	}
	if WithinAvailability(daily, day(9, 8, 0), day(9, 10, 0), loc) {
		t.Fatal("booking starting before the window was accepted")
	}
	if WithinAvailability(daily, day(9, 17, 0), day(9, 19, 0), loc) {
		t.Fatal("booking ending after the window was accepted")
	}
	if !WithinAvailability(daily, day(9, 9, 0), day(9, 18, 0), loc) {
		t.Fatal("booking covering the whole window was rejected")
	}
	if WithinAvailability(daily, day(9, 17, 0), day(10, 10, 0), loc) {
		t.Fatal("booking crossing into a day without rules was accepted")
	}

	// Monday 2026-11-09 is the only configured weekday; a Tuesday booking
	// must fail even when the clock time looks valid.
	if WithinAvailability(daily, day(10, 10, 0), day(10, 12, 0), loc) {
		t.Fatal("Tuesday booking was accepted with a Monday-only rule")
	}
}

func TestWithinAvailabilitySplitsLocalDays(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// 2026-11-09 is Monday, 2026-11-10 Tuesday.
	monday := time.Date(2026, 11, 9, 22, 0, 0, 0, loc)
	tuesday2am := time.Date(2026, 11, 10, 2, 0, 0, 0, loc)

	crossMidnight := []AvailabilityRule{
		{Weekday: 1, StartMinute: 18 * 60, EndMinute: 1440},
		{Weekday: 2, StartMinute: 0, EndMinute: 6 * 60},
	}
	if !WithinAvailability(crossMidnight, monday, tuesday2am, loc) {
		t.Fatal("cross-midnight booking covered on both days was rejected")
	}
	if WithinAvailability(crossMidnight[:1], monday, tuesday2am, loc) {
		t.Fatal("cross-midnight booking with no Tuesday rule was accepted")
	}

	// A UTC instant that belongs to the next local day must be matched against
	// the local weekday, not the UTC one.
	utcStart := time.Date(2026, 11, 9, 16, 30, 0, 0, time.UTC) // 2026-11-10 00:30 Shanghai
	utcEnd := utcStart.Add(90 * time.Minute)
	tuesdayAllDay := []AvailabilityRule{{Weekday: 2, StartMinute: 0, EndMinute: 1440}}
	if !WithinAvailability(tuesdayAllDay, utcStart, utcEnd, loc) {
		t.Fatal("local-day matching failed for a UTC input")
	}
}

func TestWithinAvailabilityMultiDay(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 11, 9, 9, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 2) // three local days
	rules := []AvailabilityRule{
		{Weekday: 1, StartMinute: 0, EndMinute: 1440},
		{Weekday: 2, StartMinute: 0, EndMinute: 1440},
		{Weekday: 3, StartMinute: 0, EndMinute: 1440},
	}
	if !WithinAvailability(rules, start, end, loc) {
		t.Fatal("multi-day booking covered every day was rejected")
	}
	if WithinAvailability(rules[:2], start, end, loc) {
		t.Fatal("multi-day booking with an uncovered final day was accepted")
	}
}

func TestWithinAvailabilityDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	allDay := []AvailabilityRule{{Weekday: 7, StartMinute: 0, EndMinute: 1440}}
	// 2026-03-08: clocks jump from 02:00 to 03:00 (23-hour day).
	start := time.Date(2026, 3, 8, 1, 30, 0, 0, ny)
	end := time.Date(2026, 3, 8, 4, 0, 0, 0, ny)
	if !WithinAvailability(allDay, start, end, ny) {
		t.Fatal("spring-forward booking within an all-day rule was rejected")
	}
	if gap := end.Sub(start); gap != 90*time.Minute {
		t.Fatalf("spring-forward span = %s, want 1h30m of elapsed time", gap)
	}

	// Windowed rule on the DST day must use wall-clock boundaries.
	window := []AvailabilityRule{{Weekday: 7, StartMinute: 9 * 60, EndMinute: 18 * 60}}
	if !WithinAvailability(window, time.Date(2026, 3, 8, 9, 0, 0, 0, ny), time.Date(2026, 3, 8, 11, 0, 0, 0, ny), ny) {
		t.Fatal("09:00-11:00 wall-clock booking on the DST day was rejected")
	}
}

func TestValidateRules(t *testing.T) {
	if err := ValidateRules(nil); err != nil {
		t.Fatalf("empty rules must be valid: %v", err)
	}
	if err := ValidateRules([]AvailabilityRule{{Weekday: 1, StartMinute: 0, EndMinute: 1440}}); err != nil {
		t.Fatalf("all-day rule rejected: %v", err)
	}
	if err := ValidateRules([]AvailabilityRule{
		{Weekday: 1, StartMinute: 540, EndMinute: 720},
		{Weekday: 1, StartMinute: 720, EndMinute: 900},
	}); err != nil {
		t.Fatalf("adjacent windows must be valid: %v", err)
	}
	bad := []AvailabilityRule{
		{Weekday: 0, StartMinute: 0, EndMinute: 60},
		{Weekday: 8, StartMinute: 0, EndMinute: 60},
		{Weekday: 1, StartMinute: -1, EndMinute: 60},
		{Weekday: 1, StartMinute: 0, EndMinute: 0},
		{Weekday: 1, StartMinute: 60, EndMinute: 60},
		{Weekday: 1, StartMinute: 120, EndMinute: 60},
		{Weekday: 1, StartMinute: 0, EndMinute: 1441},
	}
	for i, r := range bad {
		if err := ValidateRule(r); err == nil {
			t.Fatalf("rule %d (%+v) was accepted", i, r)
		}
	}
	if err := ValidateRules([]AvailabilityRule{
		{Weekday: 1, StartMinute: 540, EndMinute: 720},
		{Weekday: 1, StartMinute: 700, EndMinute: 900},
	}); err == nil {
		t.Fatal("overlapping windows were accepted")
	}
}

func TestValidateWindow(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 11, 9, 9, 0, 0, 0, loc)
	if err := ValidateWindow(start, start.Add(2*time.Hour), loc); err != nil {
		t.Fatalf("valid window rejected: %v", err)
	}
	if err := ValidateWindow(start, start, loc); err == nil {
		t.Fatal("zero-length window was accepted")
	}
	if err := ValidateWindow(start, start.Add(-time.Hour), loc); err == nil {
		t.Fatal("inverted window was accepted")
	}
	if err := ValidateWindow(start, start.Add(MaxBookingDuration), loc); err != nil {
		t.Fatalf("exactly seven days must be allowed: %v", err)
	}
	if err := ValidateWindow(start, start.Add(MaxBookingDuration+time.Second), loc); err == nil {
		t.Fatal("eight-day window was accepted")
	}
	if err := ValidateWindow(time.Time{}, start, loc); err == nil {
		t.Fatal("zero start was accepted")
	}

	// Seven local days across a fall-back transition last 169 hours of elapsed
	// time but are still seven days on the wall clock.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	dstStart := time.Date(2026, 10, 26, 9, 0, 0, 0, ny)
	dstEnd := dstStart.AddDate(0, 0, 7)
	if err := ValidateWindow(dstStart, dstEnd, ny); err != nil {
		t.Fatalf("seven wall-clock days across fall-back rejected: %v", err)
	}
}

func TestBlockRange(t *testing.T) {
	start := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	gotStart, gotEnd := BlockRange(start, end, 30, 45)
	if want := start.Add(-30 * time.Minute); !gotStart.Equal(want) {
		t.Fatalf("block start = %s, want %s", gotStart, want)
	}
	if want := end.Add(45 * time.Minute); !gotEnd.Equal(want) {
		t.Fatalf("block end = %s, want %s", gotEnd, want)
	}
}
