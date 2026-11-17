package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseServerURL(t *testing.T) {
	ok := map[string]string{
		"https://photo.example.com/":    "https://photo.example.com",
		"https://photo.example.com/api": "https://photo.example.com/api",
		"http://localhost:8080":         "http://localhost:8080",
		"http://127.0.0.1:8080/":        "http://127.0.0.1:8080",
	}
	for in, want := range ok {
		u, err := ParseServerURL(in)
		if err != nil {
			t.Fatalf("ParseServerURL(%q): %v", in, err)
		}
		if u.String() != want {
			t.Fatalf("ParseServerURL(%q) = %q, want %q", in, u.String(), want)
		}
	}
	bad := []string{
		"", "ftp://example.com", "http://photo.example.com", "https://",
		"https://user:pass@example.com", "not a url",
	}
	for _, in := range bad {
		if _, err := ParseServerURL(in); err == nil {
			t.Fatalf("ParseServerURL(%q) should fail", in)
		}
	}
}

func TestDoJSONRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSONTest(w, map[string]string{"ok": "yes"})
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, Options{})
	var out struct {
		OK string `json:"ok"`
	}
	if err := c.doJSON(context.Background(), http.MethodGet, "/x", nil, &out, "", false); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if out.OK != "yes" || calls.Load() != 3 {
		t.Fatalf("out = %+v calls = %d", out, calls.Load())
	}
}

func TestDoJSONRefreshOnUnauthorizedOnce(t *testing.T) {
	var calls atomic.Int32
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fresh" {
			t.Errorf("authorization = %q", got)
		}
		writeJSONTest(w, map[string]string{"ok": "yes"})
	}))
	defer srv.Close()
	var token atomic.Value
	token.Store("stale")
	c := newTestClient(t, srv.URL, Options{
		Token: func(context.Context) (string, error) { return token.Load().(string), nil },
		OnUnauthorized: func(context.Context) error {
			refreshes.Add(1)
			token.Store("fresh")
			return nil
		},
	})
	var out map[string]string
	if err := c.doJSON(context.Background(), http.MethodGet, "/x", nil, &out, "", true); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if refreshes.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("refreshes=%d calls=%d", refreshes.Load(), calls.Load())
	}
}

func TestProblemDecoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "batch_closed", "title": "Conflict", "detail": "sealed",
			"status": 409, "request_id": "req-1",
		})
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, Options{})
	err := c.doJSON(context.Background(), http.MethodPost, "/x", map[string]string{"a": "b"}, nil, "", false)
	if !Is(err, http.StatusConflict, "batch_closed") {
		t.Fatalf("err = %v", err)
	}
	var ae *Error
	if !errors.As(err, &ae) || ae.Detail != "sealed" || ae.RequestID != "req-1" {
		t.Fatalf("problem = %+v", err)
	}
	if !ae.Permanent() {
		t.Fatal("409 must be permanent")
	}
	if (&Error{Status: 503}).Permanent() {
		t.Fatal("503 must be retryable")
	}
}

func TestPutObjectNormalizesETagAndPresignExpiry(t *testing.T) {
	var allow atomic.Bool
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(data))
		mu.Unlock()
		if !allow.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", `W/"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, Options{})
	open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("payload")), nil }

	_, err := c.PutObject(context.Background(), srv.URL+"/obj", 7, open)
	if !Is(err, http.StatusForbidden, "presign_expired") {
		t.Fatalf("expected presign_expired, got %v", err)
	}
	allow.Store(true)
	etag, err := c.PutObject(context.Background(), srv.URL+"/obj", 7, open)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if etag != "abc123" {
		t.Fatalf("etag = %q", etag)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "payload" || got[1] != "payload" {
		t.Fatalf("body must be re-sent per attempt: %v", got)
	}
}

func TestPutObjectRejectsMissingETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, Options{})
	_, err := c.PutObject(context.Background(), srv.URL+"/obj", 1,
		func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("x")), nil })
	if err == nil {
		t.Fatal("missing etag must fail")
	}
}

func TestValidatePresignURL(t *testing.T) {
	good := []string{"https://oss.example.com/a", "http://127.0.0.1:9000/a", "http://localhost:9000/a"}
	for _, raw := range good {
		if err := ValidatePresignURL(raw); err != nil {
			t.Fatalf("ValidatePresignURL(%q): %v", raw, err)
		}
	}
	bad := []string{"http://oss.example.com/a", "ftp://x/y", "https://", "https://u:p@x/y"}
	for _, raw := range bad {
		if err := ValidatePresignURL(raw); err == nil {
			t.Fatalf("ValidatePresignURL(%q) should fail", raw)
		}
	}
}

func TestBandwidthUnlimitedAndPacing(t *testing.T) {
	if NewBandwidth(0) != nil || NewBandwidth(-5) != nil {
		t.Fatal("zero/negative bandwidth must mean unlimited")
	}
	b := NewBandwidth(100_000) // 100 KB/s
	start := time.Now()
	if err := b.wait(context.Background(), 2_000); err != nil { // 20 ms
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("pacing too fast: %v", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.wait(ctx, 1_000_000); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v", err)
	}
}

func newTestClient(t *testing.T, base string, opts Options) *Client {
	t.Helper()
	if opts.Retry.MaxAttempts == 0 {
		opts.Retry = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	}
	c, err := New(base, opts)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- contract test ----------

// TestDTOsMatchOpenAPI keeps the hand-written DTOs in lockstep with the shared
// contract: every JSON field the agent sends or reads must exist in the spec,
// and every endpoint the client calls must be declared.
func TestDTOsMatchOpenAPI(t *testing.T) {
	path := filepath.Join("..", "..", "..", "shared", "openapi.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("openapi.yaml unavailable: %v", err)
	}
	var spec struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}

	schemas := map[string]any{
		"LoginRequest":              LoginRequest{},
		"RefreshRequest":            RefreshRequest{},
		"User":                      User{},
		"Tenant":                    Tenant{},
		"Session":                   Session{},
		"Profile":                   Profile{},
		"Project":                   Project{},
		"ProjectList":               ProjectList{},
		"DeviceRegistrationRequest": DeviceRegistrationRequest{},
		"Device":                    Device{},
		"HeartbeatRequest":          HeartbeatRequest{},
		"HeartbeatResponse":         HeartbeatResponse{},
		"Batch":                     Batch{},
		"CreateBatchRequest":        CreateBatchRequest{},
		"FileInput":                 FileInput{},
		"AssetResult":               AssetResult{},
		"RegisterResult":            RegisterResult{},
		"PresignRequest":            PresignRequest{},
		"UploadPlan":                UploadPlan{},
		"CompletePart":              CompletePart{},
		"CompleteUploadRequest":     CompleteUploadRequest{},
		"Asset":                     Asset{},
		"AssetList":                 AssetList{},
		"Job":                       Job{},
		"AssetCounts":               AssetCounts{},
		"BatchStatus":               BatchStatus{},
		"DerivativePresignRequest":  DerivativePresignRequest{},
		"DerivativePresign":         DerivativePresign{},
		"DerivativeCompleteRequest": DerivativeCompleteRequest{},
		"Derivative":                Derivative{},
	}
	for name, v := range schemas {
		schema, ok := spec.Components.Schemas[name]
		if !ok {
			t.Fatalf("schema %s missing from openapi.yaml", name)
		}
		for _, field := range jsonFields(v) {
			if _, ok := schema.Properties[field]; !ok {
				t.Errorf("%s field %q is not declared in openapi.yaml", name, field)
			}
		}
	}

	paths := []string{
		"/api/v1/auth/login", "/api/v1/auth/refresh", "/api/v1/auth/logout",
		"/api/v1/auth/me", "/api/v1/projects", "/api/v1/projects/{projectID}/assets",
		"/agent/v1/devices", "/agent/v1/heartbeats",
		"/agent/v1/ingest-batches", "/agent/v1/ingest-batches/{batchID}/assets/batch",
		"/agent/v1/ingest-batches/{batchID}/finalize", "/agent/v1/ingest-batches/{batchID}",
		"/agent/v1/jobs/{jobID}",
		"/agent/v1/uploads/presign", "/agent/v1/uploads/{assetID}/complete",
		"/agent/v1/derivatives:presign", "/agent/v1/derivatives/{assetID}:complete",
	}
	for _, p := range paths {
		if _, ok := spec.Paths[p]; !ok {
			t.Errorf("path %s missing from openapi.yaml", p)
		}
	}
}

func jsonFields(v any) []string {
	typ := reflect.TypeOf(v)
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// TestEndpointsAgainstStub exercises the real request shapes the upload flow
// depends on, including the path-escaped asset and batch ids.
func TestEndpointsAgainstStub(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/assets"):
			writeJSONTest(w, AssetList{Items: []Asset{}, Total: 0})
		case strings.Contains(r.URL.Path, "/presign"):
			writeJSONTest(w, UploadPlan{Mode: "single", AssetID: "a", URL: "https://oss/x"})
		default:
			writeJSONTest(w, map[string]string{})
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, Options{})
	ctx := context.Background()
	if items, total, err := c.ListAssets(ctx, "prj/1", 500, 0); err != nil || len(items) != 0 || total != 0 {
		t.Fatalf("list assets: %v %v %d", items, err, total)
	}
	if _, err := c.Presign(ctx, "k", PresignRequest{AssetID: "a", Size: 1}); err != nil {
		t.Fatalf("presign: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("seen = %v", got)
	}
	if !strings.Contains(got[0], "prj%2F1") {
		t.Fatalf("project id not escaped: %s", got[0])
	}
	if !strings.Contains(got[0], "limit=200") {
		t.Fatalf("limit must be clamped to 200: %s", got[0])
	}
}
