package lead

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestPublicLeadOriginAndTenantRequirements(t *testing.T) {
	h := NewPublicHandler(nil, uuid.Nil, []string{"https://lumeo.example"})

	request := httptest.NewRequest(http.MethodPost, "/public/leads", strings.NewReader(`{"email":"hello@example.com"}`))
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("Origin", "https://other.example")
	response := httptest.NewRecorder()
	h.submit(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("origin status = %d, want 403", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/public/leads", strings.NewReader(`{"email":"hello@example.com"}`))
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("Origin", "https://lumeo.example")
	response = httptest.NewRecorder()
	h.submit(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing tenant status = %d, want 503", response.Code)
	}
}

func TestPublicLeadRoutePreflightAndValidation(t *testing.T) {
	h := NewPublicHandler(nil, uuid.Nil, []string{"https://lumeo.example"})
	router := chi.NewRouter()
	h.Register(router)

	preflight := httptest.NewRequest(http.MethodOptions, "/public/leads", nil)
	preflight.Header.Set("Origin", "https://lumeo.example")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", response.Code)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://lumeo.example" {
		t.Fatalf("allow origin = %q", response.Header().Get("Access-Control-Allow-Origin"))
	}

	request := httptest.NewRequest(http.MethodPost, "/public/leads", strings.NewReader(`{"email":"bad"}`))
	request.RemoteAddr = "192.0.2.4:1234"
	request.Header.Set("Origin", "https://lumeo.example")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid email status = %d, want 422", response.Code)
	}
}

func TestPublicLeadHoneypotAndRateLimit(t *testing.T) {
	h := NewPublicHandler(nil, uuid.Nil, []string{"https://lumeo.example"})
	for i := 0; i < 5; i++ {
		request := httptest.NewRequest(http.MethodPost, "/public/leads", strings.NewReader(`{"email":"bot@example.com","website":"spam"}`))
		request.RemoteAddr = "192.0.2.2:1234"
		request.Header.Set("Origin", "https://lumeo.example")
		response := httptest.NewRecorder()
		h.submit(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("honeypot status = %d, want 202", response.Code)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/public/leads", strings.NewReader(`{"email":"bot@example.com"}`))
	request.RemoteAddr = "192.0.2.2:1234"
	request.Header.Set("Origin", "https://lumeo.example")
	response := httptest.NewRecorder()
	h.submit(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("sixth request status = %d, want 429", response.Code)
	}
	if h.allow("192.0.2.2", time.Now().Add(time.Minute)) != true {
		t.Fatal("rate window did not reset")
	}
}

func TestValidEmail(t *testing.T) {
	for _, value := range []string{"person@example.com", "first.last+tag@example.co.uk"} {
		if !validEmail(value) {
			t.Errorf("validEmail(%q) = false", value)
		}
	}
	for _, value := range []string{"", "not-an-email", "Name <person@example.com>", "person@example.com\n"} {
		if validEmail(value) {
			t.Errorf("validEmail(%q) = true", value)
		}
	}
}
