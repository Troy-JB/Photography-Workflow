package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"photography-workflow/agent/internal/api"
)

func sessionJSON(access string) map[string]any {
	now := time.Now()
	return map[string]any{
		"access_token":       access,
		"access_expires_at":  now.Add(time.Hour),
		"refresh_token":      "refresh-" + access,
		"refresh_expires_at": now.Add(24 * time.Hour),
		"user":               map[string]string{"id": "u1", "email": "studio@example.com", "name": "Studio"},
		"tenant":             map[string]string{"id": "t1", "slug": "studio", "name": "Studio Co"},
		"role":               "owner",
	}
}

type stubServer struct {
	mu            sync.Mutex
	logins        int
	registrations int
	heartbeats    int
	heartbeatMiss int32 // first N heartbeats return 404
	loggedOut     []string
	lastDeviceID  string
}

func newStub(t *testing.T, s *stubServer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			s.mu.Lock()
			s.logins++
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(sessionJSON("access-1"))
		case "/agent/v1/devices":
			s.mu.Lock()
			s.registrations++
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.Device{
				ID: "dev-1", Fingerprint: "pwf-x", Name: "Desk", Status: "active",
			})
		case "/agent/v1/heartbeats":
			var req api.HeartbeatRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if atomic.AddInt32(&s.heartbeatMiss, -1) >= 0 {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(api.Error{Status: 404, Code: "device_not_found"})
				return
			}
			s.mu.Lock()
			s.heartbeats++
			s.lastDeviceID = req.DeviceID
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.HeartbeatResponse{
				DeviceID: req.DeviceID, ServerTime: time.Now(), Features: map[string]bool{},
			})
		case "/api/v1/auth/logout":
			var req api.RefreshRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			s.mu.Lock()
			s.loggedOut = append(s.loggedOut, req.RefreshToken)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Status: 404, Code: "not_found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openTest(t *testing.T, srv *httptest.Server) *Conn {
	t.Helper()
	conn, err := Open(Config{ServerURL: srv.URL, Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return conn
}

func TestLoginRegistersAndPersistsSession(t *testing.T) {
	stub := &stubServer{heartbeatMiss: -1}
	srv := newStub(t, stub)
	dir := t.TempDir()
	conn, err := Open(Config{ServerURL: srv.URL, Dir: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if _, err := conn.Login(ctx, "studio@example.com", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if info := conn.Describe(); !info.LoggedIn || info.Email != "studio@example.com" || info.Role != "owner" {
		t.Fatalf("describe = %+v", info)
	}
	if _, err := conn.Register(ctx); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := conn.DeviceID(); got != "dev-1" {
		t.Fatalf("device id = %q", got)
	}

	// A restart must restore the session and the device id from disk.
	again, err := Open(Config{ServerURL: srv.URL, Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !again.LoggedIn() {
		t.Fatal("session must survive a restart")
	}
	if again.DeviceID() != "dev-1" {
		t.Fatalf("device id after restart = %q", again.DeviceID())
	}
	if again.Describe().Fingerprint != conn.Describe().Fingerprint {
		t.Fatal("fingerprint must be stable")
	}
}

func TestLoginClearsPreviousTenantDeviceID(t *testing.T) {
	stub := &stubServer{heartbeatMiss: -1}
	srv := newStub(t, stub)
	conn := openTest(t, srv)
	ctx := context.Background()
	if _, err := conn.Login(ctx, "studio@example.com", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := conn.Register(ctx); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := conn.Device().SetServerID("dev-from-another-tenant"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Login(ctx, "studio@example.com", "pw"); err != nil {
		t.Fatalf("relogin: %v", err)
	}
	if conn.DeviceID() != "" {
		t.Fatalf("stale device id survived login: %q", conn.DeviceID())
	}
	if id, err := conn.EnsureDevice(ctx); err != nil || id != "dev-1" {
		t.Fatalf("ensure device = %q, %v", id, err)
	}
}

func TestHeartbeatReregistersAfterNotFound(t *testing.T) {
	stub := &stubServer{heartbeatMiss: 1}
	srv := newStub(t, stub)
	conn := openTest(t, srv)
	ctx := context.Background()
	if _, err := conn.Login(ctx, "studio@example.com", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := conn.Register(ctx); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := conn.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	stub.mu.Lock()
	regs := stub.registrations
	beats := stub.heartbeats
	id := stub.lastDeviceID
	stub.mu.Unlock()
	if regs != 2 {
		t.Fatalf("registrations = %d, want 2 (re-register after 404)", regs)
	}
	if beats != 1 || id != "dev-1" {
		t.Fatalf("beats = %d, last id = %q", beats, id)
	}
}

func TestLogoutRevokesAndForgets(t *testing.T) {
	stub := &stubServer{heartbeatMiss: -1}
	srv := newStub(t, stub)
	conn := openTest(t, srv)
	ctx := context.Background()
	if _, err := conn.Login(ctx, "studio@example.com", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := conn.Logout(ctx); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if conn.LoggedIn() {
		t.Fatal("session must be forgotten locally")
	}
	stub.mu.Lock()
	revoked := append([]string(nil), stub.loggedOut...)
	stub.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "refresh-access-1" {
		t.Fatalf("revoked = %v", revoked)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(api.Error{Status: 401, Code: "unauthorized"})
	}))
	defer srv.Close()
	conn := openTest(t, srv)
	_, err := conn.Login(context.Background(), "a@b.c", "wrong")
	if err == nil || LoginError(err).Error() != "邮箱或密码错误" {
		t.Fatalf("login error = %v", err)
	}
	if conn.LoggedIn() {
		t.Fatal("failed login must not create a session")
	}
}

func TestOpenRejectsMissingServer(t *testing.T) {
	if _, err := Open(Config{Dir: t.TempDir()}); err != ErrNoServer {
		t.Fatalf("err = %v", err)
	}
	if _, err := Open(Config{ServerURL: "http://example.com", Dir: t.TempDir()}); err == nil {
		t.Fatal("remote plain HTTP must be rejected")
	}
}
