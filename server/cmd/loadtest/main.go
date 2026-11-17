package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type config struct {
	Base        string
	Email       string
	Password    string
	Scenario    string
	Concurrency int
	Duration    time.Duration
	Think       time.Duration
	P95         time.Duration
	P99         time.Duration
	ErrorRate   float64
	Timeout     time.Duration
	JobTimeout  time.Duration
	JSONOut     string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.Base, "base", envOr("PWF_LOADTEST_BASE", "http://127.0.0.1:8080"), "API base URL")
	flag.StringVar(&cfg.Email, "email", "owner@example.com", "login email")
	flag.StringVar(&cfg.Password, "password", "photographer123", "login password")
	flag.StringVar(&cfg.Scenario, "scenario", "ordinary-api",
		"selection-write | ordinary-api | booking-200")
	flag.IntVar(&cfg.Concurrency, "concurrency", 20,
		"parallel clients (booking-200: competing holds)")
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second,
		"run time for selection-write/ordinary-api (ignored by booking-200)")
	flag.DurationVar(&cfg.Think, "think", 0,
		"pause between iterations (selection-write defaults to 1100ms to stay under the 120/min session budget)")
	flag.DurationVar(&cfg.P95, "p95", 500*time.Millisecond, "p95 latency threshold")
	flag.DurationVar(&cfg.P99, "p99", 900*time.Millisecond, "p99 latency threshold")
	flag.Float64Var(&cfg.ErrorRate, "error-rate", 0.01, "allowed fraction of unexpected errors (0.01 = 1%)")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "per-request timeout")
	flag.DurationVar(&cfg.JobTimeout, "job-timeout", 120*time.Second, "how long to wait for ingest/derive jobs")
	flag.StringVar(&cfg.JSONOut, "json-out", "", "optional path for a machine-readable report")
	flag.Parse()

	if err := validateConfig(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	rep, err := run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(1)
	}
	rep.print()
	if err := writeJSON(cfg.JSONOut, rep); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: write %s: %v\n", cfg.JSONOut, err)
		os.Exit(1)
	}
	if !rep.Passed {
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func validateConfig(cfg *config) error {
	u, err := url.Parse(cfg.Base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("-base must be an absolute URL, got %q", cfg.Base)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("-base must use http or https, got %q", u.Scheme)
	}
	switch cfg.Scenario {
	case "ordinary-api", "selection-write", "booking-200":
	default:
		return fmt.Errorf("-scenario must be selection-write, ordinary-api or booking-200, got %q", cfg.Scenario)
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > 1000 {
		return fmt.Errorf("-concurrency must be between 1 and 1000")
	}
	if cfg.Scenario != "booking-200" && cfg.Duration <= 0 {
		return fmt.Errorf("-duration must be positive")
	}
	if cfg.P95 <= 0 || cfg.P99 <= 0 || cfg.P95 > cfg.P99 {
		return fmt.Errorf("-p95 must be positive and no larger than -p99")
	}
	if cfg.ErrorRate < 0 || cfg.ErrorRate > 1 {
		return fmt.Errorf("-error-rate must be between 0 and 1")
	}
	if cfg.Scenario == "selection-write" && cfg.Think == 0 {
		cfg.Think = 1100 * time.Millisecond
	}
	return nil
}

func run(ctx context.Context, cfg config) (*report, error) {
	client := newAPIClient(cfg.Base, cfg.Timeout)
	token, err := client.login(ctx, cfg.Email, cfg.Password)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	switch cfg.Scenario {
	case "ordinary-api":
		return runOrdinaryAPI(ctx, client, token, cfg)
	case "selection-write":
		return runSelectionWrite(ctx, client, token, cfg)
	case "booking-200":
		return runBookingRace(ctx, client, token, cfg)
	}
	return nil, fmt.Errorf("unsupported scenario %q", cfg.Scenario)
}

func newReport(cfg config) *report {
	return &report{
		Scenario:       cfg.Scenario,
		Base:           cfg.Base,
		Concurrency:    cfg.Concurrency,
		Details:        map[string]any{},
		Endpoints:      map[string]int{},
		P95ThresholdMs: float64(cfg.P95) / float64(time.Millisecond),
		P99ThresholdMs: float64(cfg.P99) / float64(time.Millisecond),
		ErrorRateLimit: cfg.ErrorRate,
	}
}

// finish folds the metrics into the report and applies the shared thresholds.
func (rep *report) finish(m *metrics, started time.Time, cfg config) {
	lats, success, conflicts, limited, errs, endpoints := m.snapshot()
	rep.ElapsedSeconds = time.Since(started).Seconds()
	rep.Requests = len(lats)
	rep.Success = success
	rep.Conflicts = conflicts
	rep.RateLimited = limited
	rep.Errors = errs
	rep.Endpoints = endpoints
	if rep.ElapsedSeconds > 0 {
		rep.RequestsPerSec = float64(rep.Requests) / rep.ElapsedSeconds
	}
	rep.P50Ms = float64(percentile(lats, 0.50)) / float64(time.Millisecond)
	rep.P95Ms = float64(percentile(lats, 0.95)) / float64(time.Millisecond)
	rep.P99Ms = float64(percentile(lats, 0.99)) / float64(time.Millisecond)
	rep.evaluate(cfg.P95, cfg.P99, cfg.ErrorRate)
}
