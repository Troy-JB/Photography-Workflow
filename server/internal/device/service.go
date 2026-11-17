package device

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
)

var (
	ErrNotFound = errors.New("device not found")
	ErrDisabled = errors.New("device is disabled")
	ErrInvalid  = errors.New("invalid device registration")
)

// Device is the tenant-visible view of an agent installation. PublicKey stays
// server-side until device tokens land; it is never echoed to clients.
type Device struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	Fingerprint  string     `json:"fingerprint"`
	Platform     string     `json:"platform"`
	AgentVersion string     `json:"agent_version"`
	Status       string     `json:"status"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type RegisterInput struct {
	Name         string
	Fingerprint  string
	Platform     string
	AgentVersion string
	PublicKey    string
}

type HeartbeatInput struct {
	DeviceID     uuid.UUID
	AgentVersion string
}

// Heartbeat tells the agent which server behaviours it may rely on and whether
// it is too old. Feature flags are explicit so a new agent can degrade instead
// of guessing from the server version.
type Heartbeat struct {
	DeviceID        uuid.UUID       `json:"device_id"`
	ServerTime      time.Time       `json:"server_time"`
	MinAgentVersion string          `json:"min_agent_version"`
	Features        map[string]bool `json:"features"`
}

func Features() map[string]bool {
	return map[string]bool{
		"ingest_v1":          true,
		"server_derivatives": true,
		"device_tokens":      false,
	}
}

type Service struct {
	pool       *pgxpool.Pool
	minVersion string
	log        *slog.Logger
}

func NewService(pool *pgxpool.Pool, minVersion string, log *slog.Logger) *Service {
	return &Service{pool: pool, minVersion: strings.TrimSpace(minVersion), log: log}
}

const deviceColumns = `id, name, fingerprint, platform, agent_version, status,
	last_seen_at, created_at, updated_at`

// Register upserts by fingerprint. A disabled device is never reactivated by
// re-registration, and its key/metadata is not refreshed: disabling must stay
// effective until an operator clears it.
func (s *Service) Register(ctx context.Context, tenantID, actor uuid.UUID, ip string, in RegisterInput) (*Device, bool, error) {
	in, err := normalizeRegister(in)
	if err != nil {
		return nil, false, err
	}
	var (
		result  *Device
		created bool
	)
	err = db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO agent_devices
				(tenant_id, user_id, name, fingerprint, platform, agent_version, public_key)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (tenant_id, fingerprint) DO UPDATE
			SET name = EXCLUDED.name,
				platform = EXCLUDED.platform,
				agent_version = EXCLUDED.agent_version,
				public_key = EXCLUDED.public_key,
				user_id = COALESCE(agent_devices.user_id, EXCLUDED.user_id),
				updated_at = now()
			WHERE agent_devices.status = 'active'
			RETURNING `+deviceColumns+`, (xmax = 0) AS inserted`,
			tenantID, actor, in.Name, in.Fingerprint, in.Platform, in.AgentVersion, in.PublicKey)

		d, inserted, err := scanDeviceWithInserted(row)
		if err == nil {
			if err := audit.Write(ctx, tx, tenantID, &actor, "agent.device.register", "agent_device",
				d.ID.String(), ip, map[string]any{
					"fingerprint":   d.Fingerprint,
					"platform":      d.Platform,
					"agent_version": d.AgentVersion,
					"inserted":      inserted,
				}); err != nil {
				return err
			}
			result, created = d, inserted
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("register device: %w", err)
		}

		// The conflict branch was skipped: either the device is disabled or a
		// concurrent registration won. Re-read to distinguish the two.
		d, err = scanDevice(tx.QueryRow(ctx,
			`SELECT `+deviceColumns+` FROM agent_devices WHERE tenant_id = $1 AND fingerprint = $2`,
			tenantID, in.Fingerprint))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("register device: conflict without a row")
		}
		if err != nil {
			return fmt.Errorf("load device after conflict: %w", err)
		}
		if d.Status == "disabled" {
			return ErrDisabled
		}
		result = d
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, created, nil
}

func (s *Service) Heartbeat(ctx context.Context, tenantID uuid.UUID, in HeartbeatInput) (*Heartbeat, error) {
	version := strings.TrimSpace(in.AgentVersion)
	if len(version) > 64 {
		return nil, fmt.Errorf("%w: agent_version must be at most 64 characters", ErrInvalid)
	}
	now := time.Now().UTC()
	var id uuid.UUID
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE agent_devices
			SET agent_version = CASE WHEN $3 <> '' THEN $3 ELSE agent_version END,
				last_seen_at = $4,
				updated_at = $4
			WHERE id = $1 AND tenant_id = $2 AND status = 'active'
			RETURNING id`, in.DeviceID, tenantID, version, now).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("heartbeat: %w", err)
		}
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM agent_devices WHERE id = $1 AND tenant_id = $2`,
			in.DeviceID, tenantID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("load device state: %w", err)
		}
		if status == "disabled" {
			return ErrDisabled
		}
		return fmt.Errorf("heartbeat: device %s is %s", in.DeviceID, status)
	})
	if err != nil {
		return nil, err
	}
	return &Heartbeat{
		DeviceID:        id,
		ServerTime:      now,
		MinAgentVersion: s.minVersion,
		Features:        Features(),
	}, nil
}

func normalizeRegister(in RegisterInput) (RegisterInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Fingerprint = strings.TrimSpace(in.Fingerprint)
	in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
	in.AgentVersion = strings.TrimSpace(in.AgentVersion)
	in.PublicKey = strings.TrimSpace(in.PublicKey)

	if in.Name == "" || len([]rune(in.Name)) > 200 {
		return RegisterInput{}, fmt.Errorf("%w: name must be 1-200 characters", ErrInvalid)
	}
	if in.Fingerprint == "" || len(in.Fingerprint) > 256 || !printable(in.Fingerprint) {
		return RegisterInput{}, fmt.Errorf("%w: fingerprint must be 1-256 printable characters", ErrInvalid)
	}
	if len(in.Platform) > 32 || !printable(in.Platform) {
		return RegisterInput{}, fmt.Errorf("%w: platform must be at most 32 printable characters", ErrInvalid)
	}
	if in.AgentVersion == "" || len(in.AgentVersion) > 64 || !printable(in.AgentVersion) {
		return RegisterInput{}, fmt.Errorf("%w: agent_version must be 1-64 printable characters", ErrInvalid)
	}
	if err := validatePublicKey(in.PublicKey); err != nil {
		return RegisterInput{}, err
	}
	return in, nil
}

// validatePublicKey enforces the agent key contract: base64 (standard encoding)
// of a PKIX DER ECDSA P-256 public key. Accepting only one encoding keeps the
// future device-token verifier unambiguous.
func validatePublicKey(encoded string) error {
	if encoded == "" || len(encoded) > 4096 {
		return fmt.Errorf("%w: public_key is required", ErrInvalid)
	}
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("%w: public_key must be base64 PKIX DER", ErrInvalid)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return fmt.Errorf("%w: public_key is not a valid PKIX public key", ErrInvalid)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve.Params().Name != elliptic.P256().Params().Name {
		return fmt.Errorf("%w: public_key must be an ECDSA P-256 key", ErrInvalid)
	}
	return nil
}

func printable(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsControl(r)
	}) < 0
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDevice(row rowScanner) (*Device, error) {
	var d Device
	if err := row.Scan(&d.ID, &d.Name, &d.Fingerprint, &d.Platform, &d.AgentVersion,
		&d.Status, &d.LastSeenAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

func scanDeviceWithInserted(row rowScanner) (*Device, bool, error) {
	var (
		d        Device
		inserted bool
	)
	if err := row.Scan(&d.ID, &d.Name, &d.Fingerprint, &d.Platform, &d.AgentVersion,
		&d.Status, &d.LastSeenAt, &d.CreatedAt, &d.UpdatedAt, &inserted); err != nil {
		return nil, false, err
	}
	return &d, inserted, nil
}
