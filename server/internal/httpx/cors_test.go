package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCORSPreflightForConfiguredOrigin(t *testing.T) {
	handler := CORS([]string{"https://admin.example.com"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("preflight should not reach the route")
	}))
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/auth/register", nil)
	request.Header.Set("Origin", "https://admin.example.com")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "content-type,last-event-id")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Fatalf("allow-origin = %q", got)
	}
	if got := response.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Fatal("allow-methods header is missing")
	}
	if got := response.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "last-event-id") {
		t.Fatal("allow-headers header is missing")
	}
}

func TestCORSRejectsUnconfiguredPreflightOrigin(t *testing.T) {
	handler := CORS([]string{"https://admin.example.com"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("untrusted preflight should not reach the route")
	}))
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/auth/register", nil)
	request.Header.Set("Origin", "https://attacker.example")
	request.Header.Set("Access-Control-Request-Method", "POST")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unexpected allow-origin header %q", got)
	}
}

func TestCORSAllowsSameOriginStyleRequest(t *testing.T) {
	called := false
	handler := CORS(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", nil))
	if !called || response.Code != http.StatusCreated {
		t.Fatalf("route called = %v, status = %d", called, response.Code)
	}
}
