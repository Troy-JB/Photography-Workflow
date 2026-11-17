package httpx

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Metrics is a small Prometheus-text registry. It exists so M0 has observable
// request rates and latency without pulling an exporter dependency into the
// module; an OpenTelemetry exporter can replace it later.
type Metrics struct {
	mu        sync.Mutex
	requests  map[requestKey]uint64
	durations map[durationKey]*histogram
	counters  map[counterKey]uint64
}

type requestKey struct {
	Method string
	Route  string
	Status string
}

type durationKey struct {
	Method string
	Route  string
}

// counterKey identifies a named counter with its serialised label set.
type counterKey struct {
	Name   string
	Labels string
}

type histogram struct {
	Buckets []uint64
	Sum     float64
	Count   uint64
}

func NewMetrics() *Metrics {
	return &Metrics{
		requests:  make(map[requestKey]uint64),
		durations: make(map[durationKey]*histogram),
		counters:  make(map[counterKey]uint64),
	}
}

// defaultMetrics backs the package-level Inc used by domain packages that have
// no handle on the API registry.
var defaultMetrics = NewMetrics()

// DefaultMetrics returns the process-wide registry. The API serves it from
// /metrics so counters incremented by business packages are exported.
func DefaultMetrics() *Metrics { return defaultMetrics }

// Inc increments a named counter in the default registry.
func Inc(name string, labels map[string]string) { defaultMetrics.Inc(name, labels) }

// Inc increments a named counter with a stable, sorted label set so the
// exposition is deterministic.
func (m *Metrics) Inc(name string, labels map[string]string) {
	if name == "" {
		return
	}
	key := counterKey{Name: name, Labels: canonicalLabels(labels)}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters == nil {
		m.counters = make(map[counterKey]uint64)
	}
	m.counters[key]++
}

func canonicalLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[k]))
		b.WriteString(`"`)
	}
	return b.String()
}

func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m *Metrics) Observe(method, route string, status int, d time.Duration) {
	secs := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{method, route, strconv.Itoa(status)}]++
	key := durationKey{method, route}
	h := m.durations[key]
	if h == nil {
		h = &histogram{Buckets: make([]uint64, len(durationBuckets))}
		m.durations[key] = h
	}
	for i, b := range durationBuckets {
		if secs <= b {
			h.Buckets[i]++
		}
	}
	h.Sum += secs
	h.Count++
}

func (m *Metrics) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			m.Observe(r.Method, route, sw.status, time.Since(start))
		})
	}
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	fmt.Fprintln(w, "# HELP http_requests_total Total HTTP requests.")
	fmt.Fprintln(w, "# TYPE http_requests_total counter")
	reqKeys := make([]requestKey, 0, len(m.requests))
	for k := range m.requests {
		reqKeys = append(reqKeys, k)
	}
	sort.Slice(reqKeys, func(i, j int) bool {
		a, b := reqKeys[i], reqKeys[j]
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		return a.Status < b.Status
	})
	for _, k := range reqKeys {
		fmt.Fprintf(w, "http_requests_total{method=%q,route=%q,status=%q} %d\n",
			k.Method, k.Route, k.Status, m.requests[k])
	}

	fmt.Fprintln(w, "# HELP http_request_duration_seconds HTTP request latency.")
	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	durKeys := make([]durationKey, 0, len(m.durations))
	for k := range m.durations {
		durKeys = append(durKeys, k)
	}
	sort.Slice(durKeys, func(i, j int) bool {
		if durKeys[i].Route != durKeys[j].Route {
			return durKeys[i].Route < durKeys[j].Route
		}
		return durKeys[i].Method < durKeys[j].Method
	})
	for _, k := range durKeys {
		h := m.durations[k]
		for i, b := range durationBuckets {
			fmt.Fprintf(w, "http_request_duration_seconds_bucket{method=%q,route=%q,le=%q} %d\n",
				k.Method, k.Route, strconv.FormatFloat(b, 'g', -1, 64), h.Buckets[i])
		}
		fmt.Fprintf(w, "http_request_duration_seconds_bucket{method=%q,route=%q,le=\"+Inf\"} %d\n",
			k.Method, k.Route, h.Count)
		fmt.Fprintf(w, "http_request_duration_seconds_sum{method=%q,route=%q} %s\n",
			k.Method, k.Route, strconv.FormatFloat(h.Sum, 'g', -1, 64))
		fmt.Fprintf(w, "http_request_duration_seconds_count{method=%q,route=%q} %d\n",
			k.Method, k.Route, h.Count)
	}

	counterKeys := make([]counterKey, 0, len(m.counters))
	for k := range m.counters {
		counterKeys = append(counterKeys, k)
	}
	sort.Slice(counterKeys, func(i, j int) bool {
		if counterKeys[i].Name != counterKeys[j].Name {
			return counterKeys[i].Name < counterKeys[j].Name
		}
		return counterKeys[i].Labels < counterKeys[j].Labels
	})
	for _, k := range counterKeys {
		if k.Labels == "" {
			fmt.Fprintf(w, "%s %d\n", k.Name, m.counters[k])
			continue
		}
		fmt.Fprintf(w, "%s{%s} %d\n", k.Name, k.Labels, m.counters[k])
	}
}
