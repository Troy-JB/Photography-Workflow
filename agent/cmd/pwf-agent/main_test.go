package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"photography-workflow/agent/internal/settings"
	"photography-workflow/agent/internal/updater"
)

// loginStub answers the two calls cmdLogin makes: credential exchange and
// device registration. Any other request fails the test loudly.
func loginStub(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			if status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"title":"Unauthorized"}`))
				return
			}
			now := time.Now()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":       "access-1",
				"access_expires_at":  now.Add(15 * time.Minute),
				"refresh_token":      "refresh-1",
				"refresh_expires_at": now.Add(720 * time.Hour),
				"role":               "owner",
				"user":               map[string]any{"id": "u-1", "email": "owner@example.com", "name": "Owner"},
				"tenant":             map[string]any{"id": "t-1", "slug": "studio", "name": "Studio"},
			})
		case "/agent/v1/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "dev-1", "name": "TEST", "fingerprint": "pwf-test",
				"platform": "windows/amd64", "agent_version": "test", "status": "active",
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestLoginPersistsConnection(t *testing.T) {
	srv := loginStub(t, http.StatusOK)
	defer srv.Close()

	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	settingsPath := filepath.Join(dir, "settings.json")

	code := run([]string{
		"login", "-server", srv.URL, "-tenant", " studio ",
		"-email", "owner@example.com", "-password", "secret",
		"-db", db, "-settings", settingsPath,
	})
	if code != exitOK {
		t.Fatalf("login exit = %d, want %d", code, exitOK)
	}

	s, err := settings.Load(settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.ServerURL != srv.URL || s.TenantSlug != "studio" {
		t.Fatalf("connection not persisted: %+v", s)
	}

	// A second command must find the endpoint without repeating -server.
	reloaded, err := settings.Load(resolveSettingsPath("", db))
	if err != nil {
		t.Fatalf("load default settings path: %v", err)
	}
	if reloaded.ServerURL != srv.URL {
		t.Fatalf("default settings path sees %q", reloaded.ServerURL)
	}
}

func TestLoginFailureKeepsSettingsUntouched(t *testing.T) {
	srv := loginStub(t, http.StatusUnauthorized)
	defer srv.Close()

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")

	code := run([]string{
		"login", "-server", srv.URL,
		"-email", "owner@example.com", "-password", "wrong",
		"-db", filepath.Join(dir, "state.db"), "-settings", settingsPath,
	})
	if code != exitError {
		t.Fatalf("login exit = %d, want %d", code, exitError)
	}
	s, err := settings.Load(settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if s.ServerURL != "" {
		t.Fatalf("failed login must not persist endpoint, got %q", s.ServerURL)
	}
}

func TestUpdateCheckExitCodes(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("installer")
	sum := sha256.Sum256(payload)
	manifest := updater.Manifest{
		Version: "0.5.0", Channel: "dev",
		URL:    "https://download.example.com/PWF-Agent-0.5.0.exe",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)),
	}
	body, err := updater.Sign(priv, manifest)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	base := []string{
		"update-check", "-manifest", srv.URL + "/manifest.json",
		"-key", base64.StdEncoding.EncodeToString(pub), "-channel", "dev", "-json",
	}
	if code := run(append(base, "-current", "0.4.0")); code != exitIncomplete {
		t.Fatalf("available exit = %d, want %d", code, exitIncomplete)
	}
	if code := run(append(base, "-current", "0.5.0")); code != exitOK {
		t.Fatalf("up-to-date exit = %d", code)
	}
	if code := run(append(base, "-current", "0.6.0")); code != exitOK {
		t.Fatalf("newer install exit = %d", code)
	}

	// Tampering with the served bytes must fail closed.
	body = []byte(`{"manifest":{"version":"9.9.9"},"signature":"AAAA"}`)
	if code := run(append(base, "-current", "0.4.0")); code != exitError {
		t.Fatalf("tampered exit = %d, want %d", code, exitError)
	}
}
