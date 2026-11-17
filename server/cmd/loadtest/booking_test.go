package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// bookingAPI fakes the slice of the staff API that the race scenario needs. It
// serialises the holds the same way the real slot guard does: only the first
// `winners` bookings get a 201, the rest see 409 slot_taken.
type bookingAPI struct {
	mu        sync.Mutex
	winners   int
	projects  int
	holds     int
	cancelled []string
}

func (f *bookingAPI) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusCreated, map[string]any{"id": "res-1"})
	})
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Title == "" {
			http.Error(w, "bad project body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.projects++
		id := fmt.Sprintf("proj-%d", f.projects)
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusCreated, map[string]any{"id": id})
	})
	mux.HandleFunc("/api/v1/bookings", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.holds++
		won := f.holds <= f.winners
		f.mu.Unlock()
		if won {
			writeJSONResponse(w, http.StatusCreated, map[string]any{"id": "booking-1", "version": 3})
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "about:blank", "title": "slot taken", "status": http.StatusConflict, "code": "slot_taken",
		})
	})
	mux.HandleFunc("/api/v1/bookings/booking-1/cancel", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ExpectedVersion int    `json:"expected_version"`
			Reason          string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad cancel body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.cancelled = append(f.cancelled, fmt.Sprintf("%d:%s", body.ExpectedVersion, body.Reason))
		f.mu.Unlock()
		writeJSONResponse(w, http.StatusOK, map[string]any{"id": "booking-1", "version": 4})
	})
	return mux
}

func writeJSONResponse(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func raceConfig(base string, concurrency int) config {
	return config{
		Base: base, Scenario: "booking-200", Concurrency: concurrency,
		P95: 500 * time.Millisecond, P99: 900 * time.Millisecond, ErrorRate: 0.01,
		Timeout: 10 * time.Second,
	}
}

func TestBookingRaceElectsOneWinner(t *testing.T) {
	fake := &bookingAPI{winners: 1}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()

	cfg := raceConfig(srv.URL, 20)
	client := newAPIClient(cfg.Base, cfg.Timeout)
	rep, err := runBookingRace(context.Background(), client, "access-token", cfg)
	if err != nil {
		t.Fatalf("race: %v", err)
	}
	if !rep.Passed {
		t.Fatalf("race failed: %v", rep.Failures)
	}
	if rep.Details["winners"] != 1 || rep.Details["slot_taken"] != 19 {
		t.Fatalf("details = %v, want winners=1 slot_taken=19", rep.Details)
	}
	fake.mu.Lock()
	cancelled := append([]string(nil), fake.cancelled...)
	fake.mu.Unlock()
	if len(cancelled) != 1 || !strings.HasPrefix(cancelled[0], "3:") {
		t.Fatalf("winner cancel = %v, want one cancel with expected_version 3", cancelled)
	}
}

func TestBookingRaceRejectsDoubleWinner(t *testing.T) {
	fake := &bookingAPI{winners: 2}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()

	cfg := raceConfig(srv.URL, 10)
	client := newAPIClient(cfg.Base, cfg.Timeout)
	rep, err := runBookingRace(context.Background(), client, "access-token", cfg)
	if err != nil {
		t.Fatalf("race: %v", err)
	}
	if rep.Passed {
		t.Fatal("race passed although two holds won")
	}
	joined := strings.Join(rep.Failures, "\n")
	if !strings.Contains(joined, "2 winners") {
		t.Fatalf("failures = %v, want a double-winner explanation", rep.Failures)
	}
}
