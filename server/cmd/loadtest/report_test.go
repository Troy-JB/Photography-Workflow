package main

import (
	"testing"
	"time"
)

func TestPercentileBounds(t *testing.T) {
	if got := percentile(nil, 0.95); got != 0 {
		t.Fatalf("empty percentile = %s, want 0", got)
	}
	sorted := make([]time.Duration, 101)
	for i := range sorted {
		sorted[i] = time.Duration(i) * time.Millisecond
	}
	cases := []struct {
		p    float64
		want time.Duration
	}{
		{0.50, 50 * time.Millisecond},
		{0.95, 95 * time.Millisecond},
		{0.99, 99 * time.Millisecond},
		{1, 100 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := percentile(sorted, tc.p); got != tc.want {
			t.Fatalf("p%.0f = %s, want %s", tc.p*100, got, tc.want)
		}
	}
}

func TestEvaluateThresholds(t *testing.T) {
	cfg := config{Scenario: "ordinary-api", P95: 500 * time.Millisecond, P99: 900 * time.Millisecond, ErrorRate: 0.01}
	rep := newReport(cfg)
	rep.Requests = 100
	rep.Errors = 0
	rep.P95Ms = 120
	rep.P99Ms = 300
	rep.evaluate(cfg.P95, cfg.P99, cfg.ErrorRate)
	if !rep.Passed {
		t.Fatalf("clean run failed: %v", rep.Failures)
	}

	rep = newReport(cfg)
	rep.Requests = 100
	rep.Errors = 3
	rep.P95Ms = 700
	rep.P99Ms = 1200
	rep.evaluate(cfg.P95, cfg.P99, cfg.ErrorRate)
	if rep.Passed {
		t.Fatal("run exceeding every threshold passed")
	}
	if len(rep.Failures) != 3 {
		t.Fatalf("failures = %v, want p95, p99 and error-rate entries", rep.Failures)
	}
	if rep.ErrorRate != 0.03 {
		t.Fatalf("error rate = %v, want 0.03", rep.ErrorRate)
	}

	rep = newReport(cfg)
	rep.evaluate(cfg.P95, cfg.P99, cfg.ErrorRate)
	if rep.Passed {
		t.Fatal("run without requests passed")
	}
}

func TestFinishFoldsMetrics(t *testing.T) {
	// One error in three requests is 33%, so allow it explicitly here; the
	// threshold logic itself is covered by TestEvaluateThresholds.
	cfg := config{Scenario: "ordinary-api", P95: 500 * time.Millisecond, P99: 900 * time.Millisecond, ErrorRate: 0.5}
	m := newMetrics()
	m.record("GET /api/v1/projects", 10*time.Millisecond, "success")
	m.record("GET /api/v1/projects", 20*time.Millisecond, "success")
	m.record("GET /api/v1/deliveries", 30*time.Millisecond, "error")

	rep := newReport(cfg)
	rep.finish(m, time.Now().Add(-2*time.Second), cfg)
	if rep.Requests != 3 || rep.Success != 2 || rep.Errors != 1 {
		t.Fatalf("counts = %d/%d/%d, want 3/2/1", rep.Requests, rep.Success, rep.Errors)
	}
	if rep.P50Ms != 20 {
		t.Fatalf("p50 = %vms, want 20ms", rep.P50Ms)
	}
	if rep.Endpoints["GET /api/v1/projects"] != 2 {
		t.Fatalf("endpoints = %v", rep.Endpoints)
	}
	if rep.ElapsedSeconds <= 0 {
		t.Fatalf("elapsed = %v", rep.ElapsedSeconds)
	}
	if !rep.Passed {
		t.Fatalf("finish failed a clean run: %v", rep.Failures)
	}
}
