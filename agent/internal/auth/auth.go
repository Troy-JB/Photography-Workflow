// Package auth owns the agent's session lifecycle: it keeps the access token in
// memory, persists only the rotated refresh token (DPAPI-protected on Windows)
// and coalesces concurrent refreshes so a burst of uploads triggers exactly one
// refresh call.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"photography-workflow/agent/internal/api"
)

var (
	// ErrLoginRequired means no refresh token is stored.
	ErrLoginRequired = errors.New("尚未登录服务器")
	// ErrExpired means the stored refresh token was rejected; a manual login is
	// needed.
	ErrExpired = errors.New("登录已过期，请重新登录")
)

// accessSkew refreshes slightly before expiry so an upload never starts with a
// token that dies mid-flight.
const accessSkew = 2 * time.Minute

// RefreshFunc exchanges a refresh token for a new session.
type RefreshFunc func(ctx context.Context, refreshToken string) (*api.Session, error)

// Persisted is what survives a restart. The access token deliberately does not.
type Persisted struct {
	RefreshToken     string     `json:"refresh_token"`
	RefreshExpiresAt time.Time  `json:"refresh_expires_at"`
	User             api.User   `json:"user"`
	Tenant           api.Tenant `json:"tenant"`
	Role             string     `json:"role"`
	Email            string     `json:"email,omitempty"`
}

type Store interface {
	Load() ([]byte, error)
	Save(data []byte) error
	Delete() error
}

// Manager serializes token access and refresh.
type Manager struct {
	store   Store
	refresh RefreshFunc

	mu            sync.Mutex
	access        string
	accessExpires time.Time
	persisted     Persisted
	loaded        bool

	refreshMu sync.Mutex
}

func NewManager(store Store, refresh RefreshFunc) *Manager {
	return &Manager{store: store, refresh: refresh}
}

// Session returns the cached profile if a refresh token exists.
func (m *Manager) Session() (Persisted, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return Persisted{}, false
	}
	if m.persisted.RefreshToken == "" {
		return Persisted{}, false
	}
	return m.persisted, true
}

// Load reads the persisted session eagerly so the UI can show who is signed
// in before the first authenticated request triggers a refresh.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded {
		return nil
	}
	err := m.loadLocked()
	if err != nil {
		// A corrupt file must not brick the app: treat it as signed out and
		// let the next login overwrite it.
		m.persisted = Persisted{}
		m.access = ""
		return err
	}
	return nil
}

// Token returns a valid access token, refreshing when needed.
func (m *Manager) Token(ctx context.Context) (string, error) {
	if tok := m.cachedToken(); tok != "" {
		return tok, nil
	}
	return m.refreshLocked(ctx, false)
}

// ForceRefresh is used after a 401: the cached access token is invalid even if
// its clock says otherwise.
func (m *Manager) ForceRefresh(ctx context.Context) error {
	_, err := m.refreshLocked(ctx, true)
	return err
}

func (m *Manager) cachedToken() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.access == "" || time.Until(m.accessExpires) <= accessSkew {
		return ""
	}
	return m.access
}

func (m *Manager) refreshLocked(ctx context.Context, force bool) (string, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	// Another waiter may have refreshed while this caller was queued.
	if !force {
		if tok := m.cachedToken(); tok != "" {
			return tok, nil
		}
	}

	m.mu.Lock()
	if !m.loaded {
		if err := m.loadLocked(); err != nil {
			m.mu.Unlock()
			return "", err
		}
	}
	rt := m.persisted.RefreshToken
	expires := m.persisted.RefreshExpiresAt
	m.mu.Unlock()
	if rt == "" {
		return "", ErrLoginRequired
	}
	if !expires.IsZero() && !time.Now().Before(expires) {
		m.Clear()
		return "", ErrExpired
	}

	sess, err := m.refresh(ctx, rt)
	if err != nil {
		if api.IsUnauthorized(err) {
			// The server rejected the refresh token: it is dead, not transient.
			_ = m.store.Delete()
			m.mu.Lock()
			m.persisted = Persisted{}
			m.access = ""
			m.mu.Unlock()
			return "", ErrExpired
		}
		return "", err
	}
	if err := m.SetSession(sess); err != nil {
		return "", err
	}
	return sess.AccessToken, nil
}

// SetSession adopts a freshly issued session and persists the refresh token.
func (m *Manager) SetSession(s *api.Session) error {
	if s == nil || s.RefreshToken == "" || s.AccessToken == "" {
		return errors.New("服务器返回的会话不完整")
	}
	p := Persisted{
		RefreshToken:     s.RefreshToken,
		RefreshExpiresAt: s.RefreshExpiresAt,
		User:             s.User,
		Tenant:           s.Tenant,
		Role:             s.Role,
	}
	m.mu.Lock()
	p.Email = m.persisted.Email
	m.access = s.AccessToken
	m.accessExpires = s.AccessExpiresAt
	m.persisted = p
	m.loaded = true
	m.mu.Unlock()

	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	if err := m.store.Save(data); err != nil {
		return fmt.Errorf("保存登录状态失败：%w", err)
	}
	return nil
}

// SetEmail records the login name for display; it is stored with the session.
func (m *Manager) SetEmail(email string) {
	m.mu.Lock()
	m.persisted.Email = email
	m.mu.Unlock()
}

// Clear forgets the session locally.
func (m *Manager) Clear() {
	m.mu.Lock()
	m.persisted = Persisted{}
	m.access = ""
	m.accessExpires = time.Time{}
	m.loaded = true
	m.mu.Unlock()
	_ = m.store.Delete()
}

func (m *Manager) loadLocked() error {
	m.loaded = true
	data, err := m.store.Load()
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("读取登录状态失败：%w", err)
	}
	var p Persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("登录状态已损坏：%w", err)
	}
	m.persisted = p
	return nil
}

// ---------- file-backed store ----------

// FileStore keeps one small secret per identity in dir. On Windows the bytes
// are DPAPI-encrypted for the current user; elsewhere they are written 0600,
// which exists for CI and developer machines only.
type FileStore struct {
	path    string
	entropy []byte
}

var ErrNotFound = errors.New("auth: secret not found")

func NewFileStore(dir, identity string) (*FileStore, error) {
	if identity == "" {
		return nil, errors.New("auth: identity is required")
	}
	sum := sha256.Sum256([]byte(identity))
	name := hex.EncodeToString(sum[:16]) + ".session"
	return &FileStore{path: joinSecret(dir, name), entropy: []byte(identity)}, nil
}

// Path exposes the on-disk location for diagnostics.
func (s *FileStore) Path() string { return s.path }

func (s *FileStore) Load() ([]byte, error) {
	data, err := readSecret(s.path)
	if err != nil {
		return nil, err
	}
	plain, err := unprotect(data, s.entropy)
	if err != nil {
		return nil, fmt.Errorf("解密登录状态失败：%w", err)
	}
	return plain, nil
}

func (s *FileStore) Save(data []byte) error {
	enc, err := protect(data, s.entropy)
	if err != nil {
		return fmt.Errorf("加密登录状态失败：%w", err)
	}
	return writeSecret(s.path, enc)
}

func (s *FileStore) Delete() error {
	return removeSecret(s.path)
}
