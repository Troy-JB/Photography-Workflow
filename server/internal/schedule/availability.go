package schedule

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// MaxBookingDuration caps a single booking. Longer shoots must be split so one
// row cannot silently occupy a resource for weeks.
const MaxBookingDuration = 7 * 24 * time.Hour

// AvailabilityRule is a local wall-clock window on one ISO weekday
// (1 = Monday ... 7 = Sunday). Rules are interpreted in the configured
// schedule timezone.
type AvailabilityRule struct {
	ID          uuid.UUID `json:"id,omitempty"`
	Weekday     int       `json:"weekday"`
	StartMinute int       `json:"start_minute"`
	EndMinute   int       `json:"end_minute"`
}

func ValidateRule(r AvailabilityRule) error {
	switch {
	case r.Weekday < 1 || r.Weekday > 7:
		return fmt.Errorf("%w: weekday must be 1 (Monday) to 7 (Sunday)", ErrInvalidInput)
	case r.StartMinute < 0 || r.StartMinute > 1439:
		return fmt.Errorf("%w: start_minute must be 0..1439", ErrInvalidInput)
	case r.EndMinute < 1 || r.EndMinute > 1440:
		return fmt.Errorf("%w: end_minute must be 1..1440", ErrInvalidInput)
	case r.StartMinute >= r.EndMinute:
		return fmt.Errorf("%w: start_minute must be before end_minute", ErrInvalidInput)
	}
	return nil
}

// ValidateRules rejects malformed rules and overlapping windows on the same
// weekday. Adjacent windows are allowed; overlap is not, so coverage math has
// exactly one interpretation.
func ValidateRules(rules []AvailabilityRule) error {
	if len(rules) > 50 {
		return fmt.Errorf("%w: at most 50 availability rules per resource", ErrInvalidInput)
	}
	byDay := make(map[int][]AvailabilityRule, 7)
	for _, r := range rules {
		if err := ValidateRule(r); err != nil {
			return err
		}
		byDay[r.Weekday] = append(byDay[r.Weekday], r)
	}
	for _, day := range byDay {
		sort.Slice(day, func(i, j int) bool { return day[i].StartMinute < day[j].StartMinute })
		for i := 1; i < len(day); i++ {
			if day[i].StartMinute < day[i-1].EndMinute {
				return fmt.Errorf("%w: availability windows overlap on weekday %d", ErrInvalidInput, day[i].Weekday)
			}
		}
	}
	return nil
}

// ValidateWindow checks a booking or manual-block window.
func ValidateWindow(start, end time.Time, loc *time.Location) error {
	if start.IsZero() || end.IsZero() {
		return fmt.Errorf("%w: starts_at and ends_at are required", ErrInvalidInput)
	}
	if !end.After(start) {
		return fmt.Errorf("%w: ends_at must be after starts_at", ErrInvalidInput)
	}
	// Measure the local wall clock so a DST transition does not turn a
	// seven-day booking into 167 or 169 hours.
	if wallSpan(start, end, loc) > MaxBookingDuration {
		return fmt.Errorf("%w: a booking may not exceed %s", ErrInvalidInput, MaxBookingDuration)
	}
	return nil
}

func wallSpan(start, end time.Time, loc *time.Location) time.Duration {
	if loc == nil {
		loc = time.UTC
	}
	wall := func(t time.Time) time.Time {
		t = t.In(loc)
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	return wall(end).Sub(wall(start))
}

// WithinAvailability reports whether [start, end) is fully covered by the
// resource rules. No rules means the resource is available around the clock.
// Every local day the interval touches must be covered by that weekday's
// windows, so a 22:00-02:00 booking needs both days to be open.
func WithinAvailability(rules []AvailabilityRule, start, end time.Time, loc *time.Location) bool {
	if len(rules) == 0 {
		return true
	}
	if loc == nil {
		loc = time.UTC
	}
	byDay := make(map[int][]AvailabilityRule, 7)
	for _, r := range rules {
		byDay[r.Weekday] = append(byDay[r.Weekday], r)
	}

	s, e := start.In(loc), end.In(loc)
	day := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc)
	for day.Before(e) {
		next := day.AddDate(0, 0, 1)
		segStart, segEnd := s, e
		if day.After(segStart) {
			segStart = day
		}
		if next.Before(segEnd) {
			segEnd = next
		}
		if segStart.Before(segEnd) {
			weekday := int(day.Weekday())
			if weekday == 0 {
				weekday = 7
			}
			if !covered(byDay[weekday], day, segStart, segEnd, loc) {
				return false
			}
		}
		day = next
	}
	return true
}

// covered reports whether the union of one day's windows covers [segStart, segEnd).
func covered(rules []AvailabilityRule, day, segStart, segEnd time.Time, loc *time.Location) bool {
	if len(rules) == 0 {
		return false
	}
	type span struct{ start, end time.Time }
	spans := make([]span, 0, len(rules))
	for _, r := range rules {
		// Build wall-clock boundaries so DST days still mean 09:00 local.
		spans = append(spans, span{
			start: time.Date(day.Year(), day.Month(), day.Day(), r.StartMinute/60, r.StartMinute%60, 0, 0, loc),
			end:   time.Date(day.Year(), day.Month(), day.Day(), r.EndMinute/60, r.EndMinute%60, 0, 0, loc),
		})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })

	cursor := segStart
	for _, sp := range spans {
		if !sp.end.After(cursor) {
			continue
		}
		if sp.start.After(cursor) {
			return false
		}
		cursor = sp.end
		if !cursor.Before(segEnd) {
			return true
		}
	}
	return !cursor.Before(segEnd)
}

// BlockRange expands a booking interval by the resource buffer. Buffers take
// part in conflict detection but never in availability validation.
func BlockRange(start, end time.Time, bufferBefore, bufferAfter int) (time.Time, time.Time) {
	return start.Add(-time.Duration(bufferBefore) * time.Minute),
		end.Add(time.Duration(bufferAfter) * time.Minute)
}
