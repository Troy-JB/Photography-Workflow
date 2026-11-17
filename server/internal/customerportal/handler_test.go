package customerportal

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
)

func TestRequireCustomerAcceptsOnlyCustomerPrincipal(t *testing.T) {
	secret := "customer-test-secret"
	h := &Handler{tokens: auth.NewTokens(secret, time.Hour)}
	called := false
	next := h.requireCustomer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got, ok := CustomerFrom(r.Context()); !ok || got == uuid.Nil {
			t.Fatalf("customer principal = %s, %v", got, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	clientToken, _, err := h.tokens.Issue(uuid.New(), uuid.Nil, "client")
	if err != nil {
		t.Fatal(err)
	}
	staffToken, _, err := h.tokens.Issue(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		token      string
		wantStatus int
		wantCall   bool
	}{
		{name: "customer", token: clientToken, wantStatus: http.StatusNoContent, wantCall: true},
		{name: "staff token", token: staffToken, wantStatus: http.StatusUnauthorized},
		{name: "malformed token", token: "not-a-token", wantStatus: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest(http.MethodGet, "/c/v1/me/projects", nil)
			req.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			next.ServeHTTP(response, req)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if called != test.wantCall {
				t.Fatalf("next called = %v, want %v", called, test.wantCall)
			}
		})
	}
}

func TestNormalizeAndValidatePhone(t *testing.T) {
	if got := normalizePhone(" +86 138-0013-8000 "); got != "+8613800138000" {
		t.Fatalf("normalizePhone = %q", got)
	}
	for _, value := range []string{"+8613800138000", "12125550123"} {
		if !validPhone(value) {
			t.Errorf("validPhone(%q) = false", value)
		}
	}
	for _, value := range []string{"", "13800", "1380013800x", "+"} {
		if validPhone(value) {
			t.Errorf("validPhone(%q) = true", value)
		}
	}
}

func TestPublicRequestLimiter(t *testing.T) {
	limiter := newRequestLimiter(1, time.Minute)
	called := 0
	handler := limiter.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	for i, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/c/v1/invites/resolve", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("request %d status = %d, want %d", i+1, response.Code, want)
		}
	}
	if called != 1 {
		t.Fatalf("handler calls = %d, want 1", called)
	}
}

func TestMaskPhone(t *testing.T) {
	if got := maskPhone("+8613800138000"); got != "+86****8000" {
		t.Fatalf("maskPhone = %q", got)
	}
	if got := maskPhone("1234"); got != "***" {
		t.Fatalf("short maskPhone = %q", got)
	}
}
