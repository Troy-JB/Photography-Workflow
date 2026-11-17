package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/db"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidRefresh     = errors.New("invalid refresh token")
)

type TenantSelectionError struct {
	Tenants []Tenant
}

func (e *TenantSelectionError) Error() string { return "tenant selection required" }

type Service struct {
	pool       *pgxpool.Pool
	tokens     *Tokens
	refreshTTL time.Duration
	log        *slog.Logger
}

type User struct {
	ID               uuid.UUID `json:"id"`
	Email            string    `json:"email"`
	Name             string    `json:"name"`
	PhotographerCode string    `json:"photographer_code"`
}

type Tenant struct {
	ID   uuid.UUID `json:"id"`
	Slug string    `json:"slug"`
	Name string    `json:"name"`
}

type Session struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	User             User      `json:"user"`
	Tenant           Tenant    `json:"tenant"`
	Role             string    `json:"role"`
}

type Profile struct {
	User   User   `json:"user"`
	Tenant Tenant `json:"tenant"`
	Role   string `json:"role"`
}

func NewService(pool *pgxpool.Pool, tokens *Tokens, refreshTTL time.Duration, log *slog.Logger) *Service {
	return &Service{pool: pool, tokens: tokens, refreshTTL: refreshTTL, log: log}
}

func (s *Service) Tokens() *Tokens { return s.tokens }

func (s *Service) Login(ctx context.Context, email, password, tenantSlug string) (*Session, error) {
	var (
		userID           uuid.UUID
		userEmail        string
		userName         string
		photographerCode string
		passwordHash     string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.photographer_code, u.password_hash
		FROM users u
		WHERE lower(u.email) = lower($1)`, email).
		Scan(&userID, &userEmail, &userName, &photographerCode, &passwordHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("query user: %w", err)
	}
	if !VerifyPassword(passwordHash, password) {
		return nil, ErrInvalidCredentials
	}

	rows, err := s.pool.Query(ctx, `
		SELECT m.tenant_id, t.slug, t.name, m.role
		FROM memberships m
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND ($2::text = '' OR t.slug = $2::text)
		ORDER BY m.created_at`, userID, tenantSlug)
	if err != nil {
		return nil, fmt.Errorf("query memberships: %w", err)
	}
	defer rows.Close()
	var options []Tenant
	var selected Tenant
	var role string
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name, &role); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		options = append(options, t)
		selected = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read memberships: %w", err)
	}
	if len(options) == 0 {
		return nil, ErrInvalidCredentials
	}
	if tenantSlug == "" && len(options) > 1 {
		return nil, &TenantSelectionError{Tenants: options}
	}
	return s.issueSession(ctx, userID, userEmail, userName, photographerCode, selected.ID, selected.Slug, selected.Name, role)
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	sum := sha256.Sum256([]byte(refreshToken))

	var (
		tokenID          uuid.UUID
		userID           uuid.UUID
		userEmail        string
		userName         string
		photographerCode string
		tenantID         uuid.UUID
		tenantSlug       string
		tenantName       string
		role             string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT rt.id, rt.user_id, u.email, u.name, u.photographer_code,
		       rt.tenant_id, t.slug, t.name, m.role
		FROM refresh_tokens rt
		JOIN users u ON u.id = rt.user_id
		JOIN tenants t ON t.id = rt.tenant_id
		JOIN memberships m ON m.tenant_id = rt.tenant_id AND m.user_id = rt.user_id
		WHERE rt.token_hash = $1 AND rt.revoked_at IS NULL AND rt.expires_at > now()`,
		sum[:]).
		Scan(&tokenID, &userID, &userEmail, &userName, &photographerCode, &tenantID, &tenantSlug, &tenantName, &role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidRefresh
		}
		return nil, fmt.Errorf("query refresh token: %w", err)
	}

	revoked, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, tokenID)
	if err != nil {
		return nil, fmt.Errorf("revoke refresh token: %w", err)
	}
	if revoked.RowsAffected() == 0 {
		return nil, ErrInvalidRefresh
	}
	return s.issueSession(ctx, userID, userEmail, userName, photographerCode, tenantID, tenantSlug, tenantName, role)
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	sum := sha256.Sum256([]byte(refreshToken))
	if _, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, sum[:]); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

func (s *Service) Me(ctx context.Context, userID, tenantID uuid.UUID) (*Profile, error) {
	var (
		email, name, photographerCode, slug, tenantName, role string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.email, u.name, u.photographer_code, t.slug, t.name, m.role
		FROM users u
		JOIN memberships m ON m.user_id = u.id AND m.tenant_id = $2
		JOIN tenants t ON t.id = m.tenant_id
		WHERE u.id = $1`, userID, tenantID).
		Scan(&email, &name, &photographerCode, &slug, &tenantName, &role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("query profile: %w", err)
	}
	return &Profile{
		User:   User{ID: userID, Email: email, Name: name, PhotographerCode: photographerCode},
		Tenant: Tenant{ID: tenantID, Slug: slug, Name: tenantName},
		Role:   role,
	}, nil
}

func (s *Service) issueSession(ctx context.Context, userID uuid.UUID, email, name string,
	photographerCode string, tenantID uuid.UUID, tenantSlug, tenantName, role string) (*Session, error) {
	access, accessExp, err := s.tokens.Issue(userID, tenantID, role)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("read refresh token: %w", err)
	}
	refresh := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(refresh))
	refreshExp := time.Now().Add(s.refreshTTL)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO refresh_tokens (tenant_id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`, tenantID, userID, sum[:], refreshExp); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	return &Session{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
		User:             User{ID: userID, Email: email, Name: name, PhotographerCode: photographerCode},
		Tenant:           Tenant{ID: tenantID, Slug: tenantSlug, Name: tenantName},
		Role:             role,
	}, nil
}

// TenantTx is a small helper for services that need the tenant-scoped transaction.
func (s *Service) TenantTx(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	return db.WithTenant(ctx, s.pool, tenantID, fn)
}
