package binder

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"photography-workflow/agent/internal/updater"
)

func updateFixture(t *testing.T, channel, installerBody string) (updater.Config, *httptest.Server) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(installerBody)
	sum := sha256.Sum256(payload)

	var manifestBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(manifestBody)
		case "/installer.exe":
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	m := updater.Manifest{
		Version: "0.5.0", Channel: channel, URL: srv.URL + "/installer.exe",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)),
		Notes: "fixes capture stalls", MinVersion: "0.5.0",
	}
	manifestBody, err = updater.Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	cfg := updater.Config{
		ManifestURL: srv.URL + "/manifest.json",
		PublicKey:   pub,
		Channel:     channel,
		Dir:         t.TempDir(),
	}
	if channel == "stable" {
		cfg.SignerThumbprint = "AABBCC"
	}
	return cfg, srv
}

func waitDownloadFinished(t *testing.T, app *App) UpdateState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := app.UpdateStatus()
		if !st.Downloading {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("update download did not finish within 10s")
	return UpdateState{}
}

func TestUpdateStatusNotConfiguredForDevBuild(t *testing.T) {
	app, _ := newTestApp(t)
	st := app.UpdateStatus()
	if st.Configured || st.Supported {
		t.Fatalf("dev build must not report a configured updater: %+v", st)
	}
	if st.Current == "" {
		t.Fatal("current version must always be reported")
	}
	if _, err := app.DownloadUpdate(); err == nil {
		t.Fatal("download without an update source must fail")
	}
	if _, err := app.InstallUpdate(); err == nil {
		t.Fatal("install without a downloaded package must fail")
	}
}

func TestUpdateCheckDownloadInstall(t *testing.T) {
	app, cap := newTestApp(t)
	cfg, _ := updateFixture(t, "dev", "installer-payload")
	app.mu.Lock()
	app.upd = &updateRuntime{cfg: cfg, version: "0.4.0", supported: true, root: t.TempDir()}
	app.mu.Unlock()

	st, err := app.CheckUpdate()
	if err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if !st.Available || st.Latest != "0.5.0" || !st.Mandatory {
		t.Fatalf("unexpected check state: %+v", st)
	}
	if st.CheckedAt == "" || st.Notes == "" || st.Channel != "dev" || !st.Unsigned {
		t.Fatalf("check state incomplete: %+v", st)
	}

	if _, err := app.DownloadUpdate(); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	st = waitDownloadFinished(t, app)
	if st.Error != "" || !st.Downloaded || st.Progress != 100 {
		t.Fatalf("download failed: %+v", st)
	}
	if st.DownloadedSize != int64(len("installer-payload")) {
		t.Fatalf("downloaded size = %d", st.DownloadedSize)
	}

	var launched string
	app.SetInstallerLauncher(func(path string) error {
		launched = path
		return nil
	})
	st, err = app.InstallUpdate()
	if err != nil {
		t.Fatalf("InstallUpdate: %v", err)
	}
	if !st.Installing || launched == "" {
		t.Fatalf("install not launched: %+v launched=%q", st, launched)
	}
	if filepath.Base(launched) != "PWF-Agent-0.5.0.exe" {
		t.Fatalf("launched installer = %q", launched)
	}
	if data, err := os.ReadFile(launched); err != nil || string(data) != "installer-payload" {
		t.Fatalf("launched installer content = %q err=%v", data, err)
	}

	// A second install must not start the installer again.
	if _, err := app.InstallUpdate(); err != nil {
		t.Fatalf("idempotent InstallUpdate: %v", err)
	}

	// Corrupting the downloaded file after download must block installation.
	if err := os.WriteFile(launched, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.upd.installing = false
	app.mu.Unlock()
	if _, err := app.InstallUpdate(); err == nil {
		t.Fatal("tampered installer must be rejected")
	}

	cap.mu.Lock()
	sawChange := false
	for _, n := range cap.names {
		if n == "update:changed" || n == "update:progress" {
			sawChange = true
		}
	}
	cap.mu.Unlock()
	if !sawChange {
		t.Fatal("no update events emitted")
	}
}

func TestStableChannelRejectsUnsignedInstaller(t *testing.T) {
	app, _ := newTestApp(t)
	cfg, _ := updateFixture(t, "stable", "unsigned-installer")
	app.mu.Lock()
	app.upd = &updateRuntime{cfg: cfg, version: "0.4.0", supported: true, root: t.TempDir()}
	app.mu.Unlock()

	if _, err := app.CheckUpdate(); err != nil {
		t.Fatalf("CheckUpdate: %v", err)
	}
	if _, err := app.DownloadUpdate(); err != nil {
		t.Fatalf("DownloadUpdate: %v", err)
	}
	st := waitDownloadFinished(t, app)
	if st.Error == "" || st.Downloaded {
		t.Fatalf("unsigned stable installer must fail closed: %+v", st)
	}
	entries, _ := os.ReadDir(cfg.Dir)
	if len(entries) != 0 {
		t.Fatalf("rejected installer must be removed, found %d files", len(entries))
	}
}

func TestRollbackNoticeBecomesAlert(t *testing.T) {
	dir := t.TempDir()
	noticePath := filepath.Join(dir, "update-rollback.json")
	if err := updater.WriteRollbackNotice(noticePath, "0.5.0", "0.4.0"); err != nil {
		t.Fatal(err)
	}
	cap := &capture{}
	app := New(filepath.Join(dir, "state.db"), filepath.Join(dir, "settings.json"), cap.emit)
	app.Startup(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		app.Shutdown(ctx)
	})
	if h := app.Health(); !h.OK {
		t.Fatalf("app not healthy: %+v", h)
	}
	alerts, err := app.Alerts()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range alerts {
		if a.Code == "update.rolled_back" && a.Level == "warning" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rollback alert missing: %+v", alerts)
	}
	if _, err := os.Stat(noticePath); !os.IsNotExist(err) {
		t.Fatalf("rollback notice must be consumed, stat err = %v", err)
	}
}

func TestUpdateStateReportsInFlightBytes(t *testing.T) {
	app := &App{upd: &updateRuntime{
		version:     "0.4.0-dev",
		cfg:         updater.Config{Channel: "dev"},
		downloading: true,
		downBytes:   4096,
		progress:    25,
	}}
	st := app.UpdateStatus()
	if !st.Downloading || st.Progress != 25 || st.DownloadedSize != 4096 {
		t.Fatalf("in-flight download must expose bytes and percent: %+v", st)
	}
	app.upd.downloading = false
	app.upd.downloaded = filepath.Join(t.TempDir(), "installer.exe")
	if err := os.WriteFile(app.upd.downloaded, make([]byte, 12), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := app.UpdateStatus(); st.DownloadedSize != 12 || !st.Downloaded {
		t.Fatalf("finished download must report the on-disk size: %+v", st)
	}
}
