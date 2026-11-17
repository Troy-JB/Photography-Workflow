package proof

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/media"
	"photography-workflow/server/internal/storage"
)

// Config carries the proof policy knobs. Zero values are replaced by the
// documented defaults in NewService so every caller gets a usable service.
type Config struct {
	// PublicBaseURL prefixes the one-time proof links; empty disables links.
	PublicBaseURL      string
	PresignTTL         time.Duration
	SessionTTL         time.Duration
	TokenGrace         time.Duration
	MaxAssets          int
	AnnotationMaxBytes int
	RevisionMaxBytes   int64
	// RetouchProvider names the integration used when a retouch task starts.
	RetouchProvider string
	// DefaultWatermark mirrors gallery.watermark_text; the tenant name is the
	// final fallback when resolving retouch preview watermarks.
	DefaultWatermark string
}

// Notifier queues customer email and staff notifications inside the caller's
// transaction. It is wired in 6.5 so proof never needs the SMTP client itself.
type Notifier interface {
	// EmailAvailable reports whether proof mail can be delivered at all, so the
	// explicit resend endpoint can answer 409 before minting a token.
	EmailAvailable() bool
	ProofInvite(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, round *Round, token *IssuedToken) error
	ProofResubmitted(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, round *Round, token *IssuedToken) error
	ProofDecision(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, round *Round, decision string) error
}

// SMSSender delivers a proof link over SMS. The sandbox implementation only
// records the attempt; production requires a real provider.
type SMSSender interface {
	SendProofLink(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, round *Round,
		plaintext string, expiresAt *time.Time) error
}

// Exporter hands a started retouch task to the configured provider. The manual
// provider needs no export, so the hook stays nil for it.
type Exporter interface {
	ExportTask(ctx context.Context, tx pgx.Tx, tenantID, retouchTaskID uuid.UUID) error
}

type Service struct {
	pool     *pgxpool.Pool
	store    storage.Storage
	tokens   *auth.ProofTokens
	cfg      Config
	log      *slog.Logger
	notifier Notifier
	sms      SMSSender
	exporter Exporter
}

func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

func (s *Service) SetSMS(m SMSSender) { s.sms = m }

func (s *Service) SetExporter(e Exporter) { s.exporter = e }

func NewService(pool *pgxpool.Pool, store storage.Storage, tokens *auth.ProofTokens, cfg Config, log *slog.Logger) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 4 * time.Hour
	}
	if cfg.TokenGrace <= 0 {
		cfg.TokenGrace = 168 * time.Hour
	}
	if cfg.MaxAssets <= 0 {
		cfg.MaxAssets = 500
	}
	if cfg.AnnotationMaxBytes <= 0 {
		cfg.AnnotationMaxBytes = 16384
	}
	if cfg.RevisionMaxBytes <= 0 {
		cfg.RevisionMaxBytes = 50 << 20
	}
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = 15 * time.Minute
	}
	return &Service{pool: pool, store: store, tokens: tokens, cfg: cfg, log: log}
}

// Token is the stored, safe representation of a proof link. The plaintext never
// comes back from the database.
type Token struct {
	ID         uuid.UUID  `json:"id"`
	Label      string     `json:"label"`
	MaxVisits  int        `json:"max_visits"`
	Visits     int        `json:"visits"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// IssuedToken carries the only copy of the plaintext link. Only its SHA-256 is
// stored, so it cannot be recovered afterwards; re-notifying issues a new one.
type IssuedToken struct {
	Token
	Plaintext string `json:"token"`
	ProofURL  string `json:"proof_url"`
}

type TokenInput struct {
	Label     string
	MaxVisits int
	ExpiresAt *time.Time
}

const tokenColumns = `id, label, max_visits, visits, expires_at, revoked_at, last_used_at, created_at`

func scanToken(row scanner) (*Token, error) {
	var t Token
	if err := row.Scan(&t.ID, &t.Label, &t.MaxVisits, &t.Visits, &t.ExpiresAt,
		&t.RevokedAt, &t.LastUsedAt, &t.CreatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

// issueToken inserts a hashed proof link inside the caller's transaction and
// returns the plaintext once. The default expiry follows the round deadline
// plus the configured grace period.
func (s *Service) issueToken(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, round *Round,
	actor *uuid.UUID, ip string, in TokenInput) (*IssuedToken, error) {
	label := strings.TrimSpace(in.Label)
	if len(label) > 80 {
		return nil, fmt.Errorf("%w: label must be at most 80 characters", ErrInvalidInput)
	}
	if in.MaxVisits < 0 || in.MaxVisits > 100000 {
		return nil, fmt.Errorf("%w: max_visits must be between 0 and 100000", ErrInvalidInput)
	}
	expires := in.ExpiresAt
	if expires == nil {
		d := round.DueAt.Add(s.cfg.TokenGrace)
		expires = &d
	} else if !expires.After(time.Now()) {
		return nil, fmt.Errorf("%w: expires_at must be in the future", ErrInvalidInput)
	}
	if !expires.After(round.DueAt) {
		return nil, fmt.Errorf("%w: expires_at must be after the round deadline", ErrInvalidInput)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate proof token: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))

	tok, err := scanToken(tx.QueryRow(ctx, `
		INSERT INTO proof_round_tokens
			(tenant_id, proof_round_id, token_hash, label, max_visits, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+tokenColumns,
		tenantID, round.ID, hex.EncodeToString(sum[:]), label, in.MaxVisits, expires, actor))
	if err != nil {
		return nil, fmt.Errorf("insert proof token: %w", err)
	}
	if err := audit.Write(ctx, tx, tenantID, actor, "proof.token.created", "proof_round", round.ID.String(), ip,
		map[string]any{"token_id": tok.ID, "label": label}); err != nil {
		return nil, err
	}
	return &IssuedToken{Token: *tok, Plaintext: plaintext, ProofURL: s.proofURL(plaintext)}, nil
}

func (s *Service) proofURL(token string) string {
	base := strings.TrimRight(s.cfg.PublicBaseURL, "/")
	return base + "/#/p/" + token
}

var (
	// ErrClientEmailMissing means the project has no customer address, so no
	// proof invitation can be delivered.
	ErrClientEmailMissing = errors.New("project client has no email")
	// ErrClientPhoneMissing means the customer has no phone number on file.
	ErrClientPhoneMissing = errors.New("project client has no phone")
	// ErrChannelUnavailable means the requested notification transport is not
	// configured in this deployment.
	ErrChannelUnavailable = errors.New("notification channel is not configured")
)

// NotifyCustomer issues a fresh proof link and queues the invitation. The old
// plaintext cannot be recovered, so re-sending always creates a new token.
func (s *Service) NotifyCustomer(ctx context.Context, tenantID, roundID, actor uuid.UUID, ip, channel string) (*IssuedToken, error) {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		channel = "email"
	}
	switch channel {
	case "email":
		if s.notifier == nil || !s.notifier.EmailAvailable() {
			return nil, ErrChannelUnavailable
		}
	case "sms":
		if s.sms == nil {
			return nil, ErrChannelUnavailable
		}
	default:
		return nil, fmt.Errorf("%w: channel must be email or sms", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*IssuedToken, error) {
		round, err := loadRound(ctx, tx, tenantID, roundID, true)
		if err != nil {
			return nil, err
		}
		if round.Status == StatusApproved {
			return nil, fmt.Errorf("%w: the round is already approved", ErrInvalidState)
		}
		if channel == "email" {
			// Resend uses the address on file today, not the creation snapshot.
			email, err := projectClientEmail(ctx, tx, tenantID, round.ProjectID)
			if err != nil {
				return nil, err
			}
			if email == "" {
				return nil, ErrClientEmailMissing
			}
			if email != round.ClientEmail {
				if _, err := tx.Exec(ctx, `
					UPDATE proof_rounds SET client_email = $3, updated_at = now()
					WHERE tenant_id = $1 AND id = $2`, tenantID, roundID, email); err != nil {
					return nil, fmt.Errorf("refresh proof recipient: %w", err)
				}
				round.ClientEmail = email
			}
		}
		token, err := s.issueToken(ctx, tx, tenantID, round, &actor, ip, TokenInput{Label: "notify"})
		if err != nil {
			return nil, err
		}
		if channel == "sms" {
			if err := s.sms.SendProofLink(ctx, tx, tenantID, round, token.Plaintext, token.ExpiresAt); err != nil {
				return nil, err
			}
			return token, nil
		}
		if err := s.notifier.ProofInvite(ctx, tx, tenantID, round, token); err != nil {
			return nil, err
		}
		return token, nil
	})
}

// IssueNotifyToken mints a token for a worker-side notification (deadline
// reminders). The caller owns the surrounding tenant transaction and needs the
// round for the mail template.
func (s *Service) IssueNotifyToken(ctx context.Context, tx pgx.Tx, tenantID, roundID uuid.UUID,
	label string) (*Round, *IssuedToken, error) {
	round, err := loadRound(ctx, tx, tenantID, roundID, true)
	if err != nil {
		return nil, nil, err
	}
	if round.Status == StatusApproved {
		return nil, nil, fmt.Errorf("%w: the round is already approved", ErrInvalidState)
	}
	token, err := s.issueToken(ctx, tx, tenantID, round, nil, "worker", TokenInput{Label: label})
	if err != nil {
		return nil, nil, err
	}
	return round, token, nil
}

// galleryWatermark resolves the text drawn on revision previews from the
// gallery snapshot, then the configured default, then the tenant name. It is
// resolved at enqueue time so the worker never reads galleries or tenants.
func galleryWatermark(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID, fallback string) (string, error) {
	var text string
	if err := tx.QueryRow(ctx, `
		SELECT watermark_text FROM galleries WHERE tenant_id = $1 AND id = $2`,
		tenantID, galleryID).Scan(&text); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrGalleryNotFound
		}
		return "", fmt.Errorf("load gallery watermark: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		text = fallback
	}
	return media.ResolveWatermark(ctx, tx, text, tenantID)
}
