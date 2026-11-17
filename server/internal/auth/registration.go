package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidRegistration = errors.New("invalid registration")
	ErrEmailTaken          = errors.New("email already registered")
)

type RegisterInput struct {
	Name       string `json:"name"`
	Email      string `json:"email"`
	Password   string `json:"password"`
	TenantName string `json:"tenant_name"`
}

func normalizeRegistration(in RegisterInput) (RegisterInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.TenantName = strings.TrimSpace(in.TenantName)

	if in.Name == "" || utf8.RuneCountInString(in.Name) > 120 {
		return RegisterInput{}, fmt.Errorf("name must be between 1 and 120 characters: %w", ErrInvalidRegistration)
	}
	if len(in.Email) > 254 {
		return RegisterInput{}, fmt.Errorf("email is too long: %w", ErrInvalidRegistration)
	}
	parsed, err := mail.ParseAddress(in.Email)
	if err != nil || parsed.Address != in.Email {
		return RegisterInput{}, fmt.Errorf("valid email is required: %w", ErrInvalidRegistration)
	}
	passwordLength := utf8.RuneCountInString(in.Password)
	if passwordLength < 8 || passwordLength > 128 {
		return RegisterInput{}, fmt.Errorf("password must be between 8 and 128 characters: %w", ErrInvalidRegistration)
	}
	if in.TenantName == "" || utf8.RuneCountInString(in.TenantName) > 120 {
		return RegisterInput{}, fmt.Errorf("tenant_name must be between 1 and 120 characters: %w", ErrInvalidRegistration)
	}
	return in, nil
}

func tenantSlug(name string) string {
	var prefix strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (unicode.IsLetter(r) || unicode.IsDigit(r)) && r < unicode.MaxASCII {
			prefix.WriteRune(r)
			dash = false
			continue
		}
		if prefix.Len() > 0 && !dash {
			prefix.WriteByte('-')
			dash = true
		}
	}
	base := strings.Trim(prefix.String(), "-")
	if base == "" {
		base = "studio"
	}
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "-")
	}
	return base + "-" + uuid.NewString()
}

func (s *Service) Register(ctx context.Context, input RegisterInput) (*Session, error) {
	in, err := normalizeRegistration(input)
	if err != nil {
		return nil, err
	}
	passwordHash, err := HashPassword(in.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	codeBytes := make([]byte, 8)
	if _, err := rand.Read(codeBytes); err != nil {
		return nil, fmt.Errorf("generate photographer code: %w", err)
	}
	photographerCode := "PH" + strings.ToUpper(hex.EncodeToString(codeBytes))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID, tenantID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, name, photographer_code)
		VALUES ($1, $2, $3, $4)
		RETURNING id`, in.Email, passwordHash, in.Name, photographerCode).Scan(&userID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_email_lower_key" {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}

	slug := tenantSlug(in.TenantName)
	err = tx.QueryRow(ctx, `
		INSERT INTO tenants (slug, name) VALUES ($1, $2)
		RETURNING id`, slug, in.TenantName).Scan(&tenantID)
	if err != nil {
		return nil, fmt.Errorf("insert tenant: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memberships (tenant_id, user_id, role)
		VALUES ($1, $2, 'owner')`, tenantID, userID); err != nil {
		return nil, fmt.Errorf("insert membership: %w", err)
	}

	access, accessExp, err := s.tokens.Issue(userID, tenantID, "owner")
	if err != nil {
		return nil, fmt.Errorf("issue access token: %w", err)
	}
	refreshBytes := make([]byte, 32)
	if _, err := rand.Read(refreshBytes); err != nil {
		return nil, fmt.Errorf("read refresh token: %w", err)
	}
	refresh := base64.RawURLEncoding.EncodeToString(refreshBytes)
	sum := sha256.Sum256([]byte(refresh))
	refreshExp := time.Now().Add(s.refreshTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (tenant_id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`, tenantID, userID, sum[:], refreshExp); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit registration: %w", err)
	}

	return &Session{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
		User:             User{ID: userID, Email: in.Email, Name: in.Name, PhotographerCode: photographerCode},
		Tenant:           Tenant{ID: tenantID, Slug: slug, Name: in.TenantName},
		Role:             "owner",
	}, nil
}
