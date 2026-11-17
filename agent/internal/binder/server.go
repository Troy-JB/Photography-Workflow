package binder

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"photography-workflow/agent/internal/api"
	"photography-workflow/agent/internal/auth"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/remote"
	"photography-workflow/agent/internal/settings"
)

var (
	// ErrLoginRequired tells the UI to send the operator to the server tab.
	ErrLoginRequired = errors.New("请先在“服务器”页面登录")
	// ErrProjectRequired means the import has no server project attached yet.
	ErrProjectRequired = errors.New("该导入尚未关联服务器项目")
)

// ServerState is everything the server screen and status bar render. It never
// contains tokens.
type ServerState struct {
	Configured  bool        `json:"configured"`
	Info        remote.Info `json:"info"`
	Error       string      `json:"error,omitempty"`
	DeviceError string      `json:"device_error,omitempty"`
}

// connection returns the cached connection for the current settings, building
// it on first use. Changing the server URL, tenant or bandwidth rebuilds it.
func (a *App) connection() (*remote.Conn, error) {
	s, err := settings.Load(a.settingsPath)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.ServerURL) == "" {
		return nil, remote.ErrNoServer
	}
	key := fmt.Sprintf("%s|%s|%d", s.ServerURL, s.TenantSlug, s.BandwidthKbps)

	a.mu.Lock()
	if a.remote != nil && a.remoteKey == key {
		conn := a.remote
		a.mu.Unlock()
		return conn, nil
	}
	a.mu.Unlock()

	conn, err := remote.Open(remote.Config{
		ServerURL:     s.ServerURL,
		TenantSlug:    s.TenantSlug,
		BandwidthKbps: s.BandwidthKbps,
		Dir:           filepath.Dir(a.settingsPath),
		Version:       Version,
	})
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	// Another caller may have won the race; either connection is equivalent.
	a.remote, a.remoteKey = conn, key
	a.mu.Unlock()
	return conn, nil
}

func (a *App) invalidateConnection() {
	a.mu.Lock()
	a.remote = nil
	a.remoteKey = ""
	a.mu.Unlock()
}

// prepareServer builds the cached connection (no network I/O) and, when a
// session survived the restart, starts heartbeats.
func (a *App) prepareServer() {
	conn, err := a.connection()
	if err != nil {
		if !errors.Is(err, remote.ErrNoServer) {
			a.setServerError(err.Error())
		}
		return
	}
	if conn.LoggedIn() {
		a.emitEvent("server:changed", conn.Describe())
		a.startHeartbeat(conn)
	}
}

// SaveServerConfig persists the connection settings (server URL, tenant and
// bandwidth) without requiring destination folders to be configured yet.
func (a *App) SaveServerConfig(serverURL, tenantSlug string, bandwidthKbps int) (ServerState, error) {
	if _, err := settings.SaveConnection(a.settingsPath, serverURL, tenantSlug, bandwidthKbps); err != nil {
		return ServerState{}, err
	}
	a.invalidateConnection()
	a.stopHeartbeat()
	a.prepareServer()
	return a.Server(), nil
}

// Server reports the cached connection state without touching the network.
func (a *App) Server() ServerState {
	conn, err := a.connection()
	if err != nil {
		// A fresh profile has no endpoint yet; that is onboarding, not a fault.
		if errors.Is(err, remote.ErrNoServer) {
			return ServerState{}
		}
		return ServerState{Error: err.Error()}
	}
	a.mu.Lock()
	devErr := a.serverErr
	a.mu.Unlock()
	return ServerState{Configured: true, Info: conn.Describe(), DeviceError: devErr}
}

// Login signs in, registers this installation and starts heartbeats. A device
// registration failure does not discard the session: uploads retry it.
func (a *App) Login(email, password string) (ServerState, error) {
	conn, err := a.connection()
	if err != nil {
		return ServerState{Error: err.Error()}, err
	}
	ctx := context.Background()
	if _, err := conn.Login(ctx, email, password); err != nil {
		return a.Server(), remote.LoginError(err)
	}
	deviceErr := ""
	if _, err := conn.Register(ctx); err != nil {
		deviceErr = err.Error()
	}
	a.setServerError(deviceErr)
	a.emitEvent("server:changed", conn.Describe())
	a.startHeartbeat(conn)
	return a.Server(), nil
}

// Logout revokes the refresh token when the server is reachable and always
// forgets the local session.
func (a *App) Logout() error {
	conn, err := a.connection()
	if err != nil {
		return err
	}
	a.stopHeartbeat()
	if err := conn.Logout(context.Background()); err != nil {
		a.addEvent(context.Background(), modelEvent("warning", "logout_incomplete",
			"已在本机退出，但服务器撤销登录失败："+err.Error()))
	}
	a.setServerError("")
	a.emitEvent("server:changed", conn.Describe())
	return nil
}

// Projects lists projects the signed-in user can attach imports to.
func (a *App) Projects(limit int) ([]api.Project, error) {
	conn, err := a.connection()
	if err != nil {
		return nil, err
	}
	if !conn.LoggedIn() {
		return nil, ErrLoginRequired
	}
	projects, err := conn.API().ListProjects(context.Background(), limit)
	if api.IsUnauthorized(err) {
		return nil, errors.New("登录已过期，请重新登录")
	}
	if err != nil {
		return nil, err
	}
	return projects, nil
}

// AttachProject links an import to a server project so later uploads know
// where to register assets.
func (a *App) AttachProject(importID, projectID string) error {
	st, err := a.ready()
	if err != nil {
		return err
	}
	importID = strings.TrimSpace(importID)
	projectID = strings.TrimSpace(projectID)
	if importID == "" || projectID == "" {
		return errors.New("缺少导入或项目 ID")
	}
	return st.SetImportProject(context.Background(), importID, projectID)
}

// ---------- heartbeat ----------

// heartbeatInterval is comfortably below the server's device liveness window.
const heartbeatInterval = 60 * time.Second

func (a *App) startHeartbeat(conn *remote.Conn) {
	a.mu.Lock()
	if a.hbCancel != nil {
		a.hbCancel()
		a.hbCancel = nil
	}
	base := a.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	a.hbCancel = cancel
	a.mu.Unlock()

	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		a.beat(ctx, conn)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.beat(ctx, conn)
			}
		}
	}()
}

func (a *App) stopHeartbeat() {
	a.mu.Lock()
	cancel := a.hbCancel
	a.hbCancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) beat(ctx context.Context, conn *remote.Conn) {
	if conn == nil || !conn.LoggedIn() {
		return
	}
	hbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := conn.Heartbeat(hbCtx)
	if err == nil {
		a.setServerError("")
		return
	}
	msg := err.Error()
	if api.IsUnauthorized(err) || errors.Is(err, auth.ErrExpired) || errors.Is(err, auth.ErrLoginRequired) {
		msg = "登录已过期，请重新登录"
	}
	a.setServerError(msg)
}

// modelEvent is a small constructor so server-side failures land in the same
// persistent event stream as import alerts.
func modelEvent(level, code, msg string) model.Event {
	return model.Event{Level: level, Code: code, Message: msg}
}

// setServerError records the latest server-side failure for the status bar.
// It only emits when the message changes, so a long outage is not spammed.
func (a *App) setServerError(msg string) {
	a.mu.Lock()
	changed := a.serverErr != msg
	a.serverErr = msg
	a.mu.Unlock()
	if changed && msg != "" {
		a.addEvent(context.Background(), modelEvent("warning", "server_unavailable", msg))
	}
}
