// Package remote wires everything the agent needs to talk to one server:
// the installation's device identity, the persisted session, and an API
// client whose requests carry a rotating access token. The GUI and the CLI
// share this package so login, token refresh and device registration behave
// identically in both.
package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"photography-workflow/agent/internal/api"
	"photography-workflow/agent/internal/auth"
	"photography-workflow/agent/internal/device"
)

// sessionIdentity keys the DPAPI-protected session file; the device key uses
// a different identity so a session reset never destroys the device key.
const sessionIdentity = "pwf-agent-session"

var ErrNoServer = errors.New("尚未配置服务器地址")

// Config describes one server connection.
type Config struct {
	ServerURL     string
	TenantSlug    string
	BandwidthKbps int
	// Dir holds the DPAPI-protected session and device key files.
	Dir string
	// DeviceName is shown in the server's device list; hostname by default.
	DeviceName string
	// Version is the agent build stamped into registrations and heartbeats.
	Version string
}

// Conn is an authenticated connection to one server.
type Conn struct {
	client  *api.Client
	auth    *auth.Manager
	device  *device.Identity
	tenant  string
	version string

	mu sync.Mutex
}

// Open loads or creates the local identity and returns a connection. It does
// not perform network I/O; login and registration are explicit so the UI can
// report each step.
func Open(cfg Config) (*Conn, error) {
	cfg.ServerURL = strings.TrimSpace(cfg.ServerURL)
	if cfg.ServerURL == "" {
		return nil, ErrNoServer
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("remote: 身份目录为空")
	}
	// Validate before touching the filesystem so a typo cannot leave half an
	// identity behind.
	if _, err := api.ParseServerURL(cfg.ServerURL); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(cfg.DeviceName)
	if name == "" {
		name, _ = os.Hostname()
	}
	identity, err := device.Ensure(cfg.Dir, name)
	if err != nil {
		return nil, err
	}
	tokenStore, err := auth.NewFileStore(cfg.Dir, sessionIdentity)
	if err != nil {
		return nil, err
	}

	var mgr *auth.Manager
	bw := api.NewBandwidth(int64(cfg.BandwidthKbps) * 125) // kbit/s → byte/s
	client, err := api.New(cfg.ServerURL, api.Options{
		Token:          func(ctx context.Context) (string, error) { return mgr.Token(ctx) },
		OnUnauthorized: func(ctx context.Context) error { return mgr.ForceRefresh(ctx) },
		Bandwidth:      bw,
	})
	if err != nil {
		return nil, err
	}
	mgr = auth.NewManager(tokenStore, func(ctx context.Context, refreshToken string) (*api.Session, error) {
		return client.Refresh(ctx, refreshToken)
	})
	// Best effort: a corrupt session file means signed out, not unusable.
	_ = mgr.Load()
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = api.Version
	}
	return &Conn{
		client:  client,
		auth:    mgr,
		device:  identity,
		tenant:  strings.TrimSpace(cfg.TenantSlug),
		version: version,
	}, nil
}

func (c *Conn) API() *api.Client                { return c.client }
func (c *Conn) Device() *device.Identity        { return c.device }
func (c *Conn) Session() (auth.Persisted, bool) { return c.auth.Session() }
func (c *Conn) LoggedIn() bool                  { _, ok := c.auth.Session(); return ok }

// Login exchanges credentials, persists the rotated refresh token and drops
// the previous device registration id: the id belongs to a tenant, and the
// next EnsureDevice re-registers this fingerprint under the current one.
func (c *Conn) Login(ctx context.Context, email, password string) (*api.Session, error) {
	email = strings.TrimSpace(email)
	if email == "" || password == "" {
		return nil, errors.New("请输入邮箱和密码")
	}
	sess, err := c.client.Login(ctx, email, password, c.tenant)
	if err != nil {
		return nil, err
	}
	if err := c.auth.SetSession(sess); err != nil {
		return nil, err
	}
	c.auth.SetEmail(email)
	if err := c.device.SetServerID(""); err != nil {
		return nil, err
	}
	return sess, nil
}

// Logout revokes the refresh token server-side (best effort) and forgets the
// session locally. A network failure still logs out: the token expires anyway.
func (c *Conn) Logout(ctx context.Context) error {
	var revoked error
	if p, ok := c.auth.Session(); ok && p.RefreshToken != "" {
		if err := c.client.Logout(ctx, p.RefreshToken); err != nil && !api.IsUnauthorized(err) {
			revoked = err
		}
	}
	c.auth.Clear()
	return revoked
}

// Register upserts this installation's device record and records the id the
// server assigned. Registration is idempotent by fingerprint.
func (c *Conn) Register(ctx context.Context) (*api.Device, error) {
	name := c.device.Name()
	if name == "" {
		name = "PWF Agent"
	}
	dev, err := c.client.RegisterDevice(ctx, api.DeviceRegistrationRequest{
		Name:         name,
		Fingerprint:  c.device.Fingerprint(),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		AgentVersion: c.version,
		PublicKey:    c.device.PublicKey(),
	})
	if err != nil {
		return nil, err
	}
	if err := c.device.SetServerID(dev.ID); err != nil {
		return nil, err
	}
	return dev, nil
}

// EnsureDevice returns the tenant-local device id, registering on first use.
func (c *Conn) EnsureDevice(ctx context.Context) (string, error) {
	c.mu.Lock()
	id := c.device.ServerID()
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	dev, err := c.Register(ctx)
	if err != nil {
		return "", err
	}
	return dev.ID, nil
}

// Heartbeat refreshes last_seen_at. A 404 means the stored id belongs to a
// previous tenant (or a purged record), so it re-registers and retries once.
func (c *Conn) Heartbeat(ctx context.Context) (*api.HeartbeatResponse, error) {
	id, err := c.EnsureDevice(ctx)
	if err != nil {
		return nil, err
	}
	hb, err := c.client.Heartbeat(ctx, api.HeartbeatRequest{DeviceID: id, AgentVersion: c.version})
	if api.Is(err, 404, "device_not_found") {
		if err := c.device.SetServerID(""); err != nil {
			return nil, err
		}
		if id, err = c.EnsureDevice(ctx); err != nil {
			return nil, err
		}
		return c.client.Heartbeat(ctx, api.HeartbeatRequest{DeviceID: id, AgentVersion: c.version})
	}
	return hb, err
}

// DeviceID reports the cached server id ("" before registration).
func (c *Conn) DeviceID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.device.ServerID()
}

// Describe returns display information that never includes secrets.
func (c *Conn) Describe() Info {
	info := Info{
		ServerURL:   c.client.BaseURL(),
		DeviceID:    c.DeviceID(),
		DeviceName:  c.device.Name(),
		Fingerprint: c.device.Fingerprint(),
	}
	if p, ok := c.auth.Session(); ok {
		info.LoggedIn = true
		info.Email = p.User.Email
		info.Role = p.Role
		info.TenantName = p.Tenant.Name
		info.TenantSlug = p.Tenant.Slug
		info.UserName = p.User.Name
	}
	return info
}

// Info is safe to send to the webview.
type Info struct {
	ServerURL   string `json:"server_url"`
	LoggedIn    bool   `json:"logged_in"`
	Email       string `json:"email,omitempty"`
	UserName    string `json:"user_name,omitempty"`
	TenantName  string `json:"tenant_name,omitempty"`
	TenantSlug  string `json:"tenant_slug,omitempty"`
	Role        string `json:"role,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
	DeviceName  string `json:"device_name,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// LoginError turns a server rejection into something an operator can act on.
func LoginError(err error) error {
	if err == nil {
		return nil
	}
	if api.Is(err, 401, "") {
		return fmt.Errorf("邮箱或密码错误")
	}
	if api.Is(err, 403, "") {
		return fmt.Errorf("该账号无权使用采集端：%w", err)
	}
	return err
}
