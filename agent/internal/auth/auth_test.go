package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"photography-workflow/agent/internal/api"
)

// memStore keeps the persisted blob in memory and counts mutations.
type memStore struct {
	mu      sync.Mutex
	data    []byte
	loads   int
	saves   int
	deletes int
}

func (s *memStore) Load() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.data == nil {
		return nil, ErrNotFound
	}
	return append([]byte(nil), s.data...), nil
}

func (s *memStore) Save(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.data = append([]byte(nil), data...)
	return nil
}

func (s *memStore) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	s.data = nil
	return nil
}

func (s *memStore) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...)
}

func session(access string, accessTTL, refreshTTL time.Duration) *api.Session {
	return &api.Session{
		AccessToken:      access,
		AccessExpiresAt:  time.Now().Add(accessTTL),
		RefreshToken:     "refresh-" + access,
		RefreshExpiresAt: time.Now().Add(refreshTTL),
		User:             api.User{ID: "u1", Email: "a@example.com"},
		Tenant:           api.Tenant{ID: "t1", Slug: "studio"},
		Role:             "owner",
	}
}

func TestManagerCoalescesConcurrentRefreshes(t *testing.T) {
	store := &memStore{}
	var calls atomic.Int32
	release := make(chan struct{})
	m := NewManager(store, func(ctx context.Context, rt string) (*api.Session, error) {
		calls.Add(1)
		<-release
		return session("access-1", time.Hour, 24*time.Hour), nil
	})
	if err := m.SetSession(&api.Session{
		AccessToken:      "access-0",
		RefreshToken:     "refresh-0",
		RefreshExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	const waiters = 12
	var wg sync.WaitGroup
	results := make([]string, waiters)
	errs := make([]error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.Token(context.Background())
		}(i)
	}
	// Let every waiter queue behind the coalescing lock before releasing it.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("waiter %d: %v", i, errs[i])
		}
		if results[i] != "access-1" {
			t.Fatalf("waiter %d token = %q", i, results[i])
		}
	}
}

func TestManagerUsesCachedAccessToken(t *testing.T) {
	store := &memStore{}
	var calls atomic.Int32
	m := NewManager(store, func(ctx context.Context, rt string) (*api.Session, error) {
		calls.Add(1)
		return session("fresh", time.Hour, time.Hour), nil
	})
	if err := m.SetSession(session("access-1", time.Hour, 24*time.Hour)); err != nil {
		t.Fatalf("set session: %v", err)
	}
	tok, err := m.Token(context.Background())
	if err != nil || tok != "access-1" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0", calls.Load())
	}
}

func TestManagerRefreshesWhenAccessTokenExpires(t *testing.T) {
	store := &memStore{}
	m := NewManager(store, func(ctx context.Context, rt string) (*api.Session, error) {
		if rt != "refresh-access-old" {
			t.Fatalf("refresh token = %q", rt)
		}
		return session("access-new", time.Hour, 24*time.Hour), nil
	})
	// A token inside the skew window must be treated as expired.
	if err := m.SetSession(session("access-old", time.Minute, 24*time.Hour)); err != nil {
		t.Fatalf("set session: %v", err)
	}
	tok, err := m.Token(context.Background())
	if err != nil || tok != "access-new" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	data := store.snapshot()
	var p Persisted
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("persisted: %v", err)
	}
	if p.RefreshToken != "refresh-access-new" {
		t.Fatalf("persisted refresh token = %q (rotation must be saved)", p.RefreshToken)
	}
	if p.User.Email != "a@example.com" || p.Tenant.Slug != "studio" || p.Role != "owner" {
		t.Fatalf("persisted profile = %+v", p)
	}
}

func TestManagerExpiredRefreshTokenClearsStore(t *testing.T) {
	store := &memStore{}
	m := NewManager(store, func(ctx context.Context, rt string) (*api.Session, error) {
		t.Fatal("refresh must not be attempted after local expiry")
		return nil, nil
	})
	if err := m.SetSession(&api.Session{
		AccessToken:      "access-old",
		RefreshToken:     "refresh-old",
		RefreshExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("set session: %v", err)
	}
	if _, err := m.Token(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("token error = %v", err)
	}
	if _, ok := m.Session(); ok {
		t.Fatal("session must be forgotten")
	}
	if store.snapshot() != nil {
		t.Fatal("persisted session must be deleted")
	}
}

func TestManagerRejectedRefreshTokenClearsStore(t *testing.T) {
	store := &memStore{}
	m := NewManager(store, func(ctx context.Context, rt string) (*api.Session, error) {
		return nil, &api.Error{Status: http.StatusUnauthorized, Code: "unauthorized"}
	})
	if err := m.SetSession(&api.Session{
		AccessToken:      "access-old",
		RefreshToken:     "refresh-old",
		RefreshExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("set session: %v", err)
	}
	if _, err := m.Token(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("token error = %v", err)
	}
	if _, ok := m.Session(); ok {
		t.Fatal("session must be forgotten")
	}
}

func TestManagerTransientRefreshFailureKeepsSession(t *testing.T) {
	store := &memStore{}
	boom := errors.New("network down")
	m := NewManager(store, func(ctx context.Context, rt string) (*api.Session, error) {
		return nil, boom
	})
	if err := m.SetSession(&api.Session{
		AccessToken:      "access-old",
		RefreshToken:     "refresh-old",
		RefreshExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("set session: %v", err)
	}
	if _, err := m.Token(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("token error = %v", err)
	}
	if _, ok := m.Session(); !ok {
		t.Fatal("transient failures must not log the user out")
	}
}

func TestManagerForceRefreshIgnoresCachedToken(t *testing.T) {
	store := &memStore{}
	var calls atomic.Int32
	m := NewManager(store, func(ctx context.Context, rt string) (*api.Session, error) {
		n := calls.Add(1)
		return session(fmt.Sprintf("access-%d", n+1), time.Hour, time.Hour), nil
	})
	if err := m.SetSession(session("access-1", time.Hour, 24*time.Hour)); err != nil {
		t.Fatalf("set session: %v", err)
	}
	if err := m.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
	tok, err := m.Token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok == "access-1" {
		t.Fatal("force refresh must replace the cached access token")
	}
}

func TestManagerRequiresLogin(t *testing.T) {
	m := NewManager(&memStore{}, func(ctx context.Context, rt string) (*api.Session, error) {
		t.Fatal("refresh must not be attempted without a refresh token")
		return nil, nil
	})
	if _, err := m.Token(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("token error = %v", err)
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	store, err := NewFileStore(t.TempDir(), "pwf-device-key:test")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty load error = %v", err)
	}
	if err := store.Save([]byte(`{"refresh_token":"x"}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(data) != `{"refresh_token":"x"}` {
		t.Fatalf("round trip = %q", data)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load after delete = %v", err)
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("second delete must be idempotent: %v", err)
	}
}

func TestFileStoreSeparatesIdentities(t *testing.T) {
	dir := t.TempDir()
	a, err := NewFileStore(dir, "identity-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewFileStore(dir, "identity-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Path() == b.Path() {
		t.Fatal("identities must not share a secret file")
	}
	if err := a.Save([]byte("secret-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("identity b sees identity a's secret: %v", err)
	}
}
