package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// metrics aggregates request latency and outcome counters across workers.
// Latency excludes time the worker spent sleeping between iterations.
type metrics struct {
	mu          sync.Mutex
	latencies   []time.Duration
	success     int
	conflicts   int
	rateLimited int
	errors      int
	endpoints   map[string]int
}

func newMetrics() *metrics {
	return &metrics{endpoints: make(map[string]int)}
}

func (m *metrics) record(label string, d time.Duration, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latencies = append(m.latencies, d)
	m.endpoints[label]++
	switch outcome {
	case "success":
		m.success++
	case "conflict":
		m.conflicts++
	case "rate_limited":
		m.rateLimited++
	default:
		m.errors++
	}
}

func (m *metrics) snapshot() ([]time.Duration, int, int, int, int, map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lats := make([]time.Duration, len(m.latencies))
	copy(lats, m.latencies)
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	endpoints := make(map[string]int, len(m.endpoints))
	for k, v := range m.endpoints {
		endpoints[k] = v
	}
	return lats, m.success, m.conflicts, m.rateLimited, m.errors, endpoints
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

type report struct {
	Scenario        string         `json:"scenario"`
	Base            string         `json:"base"`
	Concurrency     int            `json:"concurrency"`
	DurationSeconds float64        `json:"duration_seconds"`
	Requests        int            `json:"requests"`
	Success         int            `json:"success"`
	Conflicts       int            `json:"conflicts"`
	RateLimited     int            `json:"rate_limited"`
	Errors          int            `json:"errors"`
	ErrorRate       float64        `json:"error_rate"`
	RequestsPerSec  float64        `json:"requests_per_sec"`
	P50Ms           float64        `json:"p50_ms"`
	P95Ms           float64        `json:"p95_ms"`
	P99Ms           float64        `json:"p99_ms"`
	P95ThresholdMs  float64        `json:"p95_threshold_ms"`
	P99ThresholdMs  float64        `json:"p99_threshold_ms"`
	ErrorRateLimit  float64        `json:"error_rate_limit"`
	Endpoints       map[string]int `json:"endpoints"`
	Details         map[string]any `json:"details,omitempty"`
	Failures        []string       `json:"failures,omitempty"`
	Passed          bool           `json:"passed"`
	ElapsedSeconds  float64        `json:"elapsed_seconds"`
}

func (r *report) fail(format string, args ...any) {
	r.Failures = append(r.Failures, fmt.Sprintf(format, args...))
}

func (r *report) evaluate(p95, p99 time.Duration, errorRateLimit float64) {
	if r.Requests == 0 {
		r.fail("no requests were issued")
	}
	r.ErrorRate = 0
	if r.Requests > 0 {
		r.ErrorRate = float64(r.Errors) / float64(r.Requests)
	}
	if r.P95Ms > float64(p95.Milliseconds()) {
		r.fail("p95 %.1fms exceeds %s", r.P95Ms, p95)
	}
	if r.P99Ms > float64(p99.Milliseconds()) {
		r.fail("p99 %.1fms exceeds %s", r.P99Ms, p99)
	}
	if r.ErrorRate > errorRateLimit {
		r.fail("error rate %.2f%% exceeds %.2f%%", r.ErrorRate*100, errorRateLimit*100)
	}
	r.Passed = len(r.Failures) == 0
}

func (r *report) print() {
	fmt.Printf("scenario      %s\n", r.Scenario)
	fmt.Printf("concurrency   %d\n", r.Concurrency)
	fmt.Printf("requests      %d in %.1fs (%.1f req/s)\n", r.Requests, r.ElapsedSeconds, r.RequestsPerSec)
	fmt.Printf("outcomes      success=%d conflict=%d rate_limited=%d error=%d (error rate %.2f%%)\n",
		r.Success, r.Conflicts, r.RateLimited, r.Errors, r.ErrorRate*100)
	fmt.Printf("latency       p50=%.1fms p95=%.1fms p99=%.1fms (limits p95=%s p99=%s)\n",
		r.P50Ms, r.P95Ms, r.P99Ms,
		time.Duration(r.P95ThresholdMs)*time.Millisecond, time.Duration(r.P99ThresholdMs)*time.Millisecond)
	for k, v := range r.Details {
		fmt.Printf("%-13s %v\n", k+":", v)
	}
	if r.Passed {
		fmt.Println("verdict       PASS")
		return
	}
	fmt.Println("verdict       FAIL")
	for _, f := range r.Failures {
		fmt.Printf("  - %s\n", f)
	}
}

func writeJSON(path string, r *report) error {
	if path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
