package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type raceResult struct {
	status int
	body   []byte
	err    error
}

// runBookingRace proves the slot guard serialises competing holds: every
// client asks for the same resource and window at once, and exactly one 201 may
// win while the rest receive 409 slot_taken.
func runBookingRace(ctx context.Context, client *apiClient, token string, cfg config) (*report, error) {
	rep := newReport(cfg)
	tag := fmt.Sprintf("LT%d", time.Now().Unix()%1_000_000)

	var resource struct {
		ID string `json:"id"`
	}
	if err := client.json(ctx, http.MethodPost, "/api/v1/resources", token, map[string]any{
		"kind": "equipment", "name": "Loadtest race " + tag,
		"buffer_before_minutes": 0, "buffer_after_minutes": 0,
	}, &resource); err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	start := time.Now().UTC().AddDate(0, 0, 28)
	start = time.Date(start.Year(), start.Month(), start.Day(), 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	projects := make([]string, cfg.Concurrency)
	for i := range projects {
		var project struct {
			ID string `json:"id"`
		}
		if err := client.json(ctx, http.MethodPost, "/api/v1/projects", token,
			map[string]any{"title": fmt.Sprintf("Loadtest race %s #%d", tag, i)}, &project); err != nil {
			return nil, fmt.Errorf("create project %d: %w", i, err)
		}
		projects[i] = project.ID
	}

	m := newMetrics()
	results := make([]raceResult, len(projects))
	var wg sync.WaitGroup
	beginAll := make(chan struct{})
	started := time.Now()
	for i, projectID := range projects {
		wg.Add(1)
		go func(i int, projectID string) {
			defer wg.Done()
			<-beginAll
			begin := time.Now()
			status, raw, err := client.do(ctx, http.MethodPost, "/api/v1/bookings", token, map[string]any{
				"project_id":   projectID,
				"resource_ids": []string{resource.ID},
				"starts_at":    start,
				"ends_at":      end,
			})
			elapsed := time.Since(begin)
			results[i] = raceResult{status: status, body: raw, err: err}
			switch {
			case err != nil:
				m.record("POST /api/v1/bookings", elapsed, "error")
			case status == http.StatusCreated:
				m.record("POST /api/v1/bookings", elapsed, "success")
			case status == http.StatusConflict && problemCode(raw) == "slot_taken":
				m.record("POST /api/v1/bookings", elapsed, "conflict")
			default:
				m.record("POST /api/v1/bookings", elapsed, "error")
			}
		}(i, projectID)
	}
	close(beginAll)
	wg.Wait()
	rep.finish(m, started, cfg)

	var winnerID string
	var winnerVersion int
	wins, conflicts := 0, 0
	for i, res := range results {
		switch {
		case res.err != nil:
			rep.fail("race request %d: %v", i, res.err)
		case res.status == http.StatusCreated:
			wins++
			var booking struct {
				ID      string `json:"id"`
				Version int    `json:"version"`
			}
			if err := json.Unmarshal(res.body, &booking); err != nil {
				rep.fail("race request %d: decode winner: %v", i, err)
				continue
			}
			winnerID, winnerVersion = booking.ID, booking.Version
		case res.status == http.StatusConflict && problemCode(res.body) == "slot_taken":
			conflicts++
		default:
			rep.fail("race request %d: status %d: %s", i, res.status, truncate(res.body, 200))
		}
	}
	rep.Details["winners"] = wins
	rep.Details["slot_taken"] = conflicts
	if wins != 1 {
		rep.fail("race produced %d winners, want exactly 1", wins)
	}
	if conflicts != len(projects)-1 {
		rep.fail("race produced %d slot_taken conflicts, want %d", conflicts, len(projects)-1)
	}
	if winnerID != "" {
		if err := client.json(ctx, http.MethodPost, "/api/v1/bookings/"+winnerID+"/cancel", token,
			map[string]any{"expected_version": winnerVersion, "reason": "loadtest race cleanup"}, nil); err != nil {
			rep.fail("cancel race winner: %v", err)
		}
	}
	rep.Passed = len(rep.Failures) == 0
	return rep, nil
}
