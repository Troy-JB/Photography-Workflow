package binder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"photography-workflow/agent/internal/api"
	"photography-workflow/agent/internal/settings"
)

// serverStub is the smallest slice of the API the server screen touches.
func serverStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			now := time.Now()
			_ = json.NewEncoder(w).Encode(api.Session{
				AccessToken: "access-1", AccessExpiresAt: now.Add(time.Hour),
				RefreshToken: "refresh-1", RefreshExpiresAt: now.Add(24 * time.Hour),
				User:   api.User{ID: "u1", Email: "studio@example.com", Name: "Studio"},
				Tenant: api.Tenant{ID: "t1", Slug: "studio", Name: "Studio Co"},
				Role:   "owner",
			})
		case "/agent/v1/devices":
			_ = json.NewEncoder(w).Encode(api.Device{ID: "dev-1", Name: "Desk", Status: "active"})
		case "/agent/v1/heartbeats":
			_ = json.NewEncoder(w).Encode(api.HeartbeatResponse{DeviceID: "dev-1", ServerTime: time.Now()})
		case "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(api.ProjectList{
				Items: []api.Project{{ID: "prj-1", Code: "WK-2026", Title: "Wedding"}},
				Total: 1, Limit: 10,
			})
		case "/api/v1/auth/logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Status: 404, Code: "not_found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func saveServerSettings(t *testing.T, app *App, serverURL string) {
	t.Helper()
	s := settings.Default()
	s.RequireBackup = false
	s.ServerURL = serverURL
	if err := settings.Save(app.settingsPath, s); err != nil {
		t.Fatalf("save settings: %v", err)
	}
}

func TestServerLoginSnapshotProjectsLogout(t *testing.T) {
	app, cap := newTestApp(t)
	srv := serverStub(t)
	saveServerSettings(t, app, srv.URL)

	if state := app.Server(); !state.Configured || state.Info.LoggedIn {
		t.Fatalf("initial state = %+v", state)
	}
	state, err := app.Login("studio@example.com", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !state.Info.LoggedIn || state.Info.Email != "studio@example.com" || state.Info.DeviceID != "dev-1" {
		t.Fatalf("state after login = %+v", state)
	}
	if state.DeviceError != "" {
		t.Fatalf("device error = %q", state.DeviceError)
	}

	snap, err := app.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Server.Info.LoggedIn || snap.Server.Info.Fingerprint == "" {
		t.Fatalf("snapshot server = %+v", snap.Server)
	}
	if !snap.BackupDisabled {
		t.Fatal("single-destination settings must surface the backup warning")
	}
	if snap.Upload.Running {
		t.Fatal("no upload should run yet")
	}

	projects, err := app.Projects(10)
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if len(projects) != 1 || projects[0].Code != "WK-2026" {
		t.Fatalf("projects = %+v", projects)
	}

	if err := app.AttachProject("", "prj-1"); err == nil {
		t.Fatal("empty import id must be rejected")
	}

	if err := app.Logout(); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if app.Server().Info.LoggedIn {
		t.Fatal("logout must clear the session")
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	found := false
	for _, name := range cap.names {
		if name == "server:changed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("server:changed was not emitted: %v", cap.names)
	}
}

func TestUploadRequiresLogin(t *testing.T) {
	app, _ := newTestApp(t)
	srv := serverStub(t)
	saveServerSettings(t, app, srv.URL)
	if err := app.StartUpload("anything"); err != ErrLoginRequired {
		t.Fatalf("start upload without login = %v", err)
	}
	if err := app.PauseUpload(); err == nil {
		t.Fatal("pause without a running upload must fail")
	}
}

func TestServerNotConfigured(t *testing.T) {
	app, _ := newTestApp(t)
	if _, err := app.Projects(10); err == nil {
		t.Fatal("projects without a server must fail")
	}
	if state := app.Server(); state.Configured || state.Error != "" {
		t.Fatalf("fresh profile must not report a connection error: %+v", state)
	}
}
