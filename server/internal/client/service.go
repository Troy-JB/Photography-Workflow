package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
)

var (
	ErrNotFound        = errors.New("client not found")
	ErrVersionConflict = errors.New("client version conflict")
	ErrInvalidInput    = errors.New("invalid client input")
)

type Client struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Company   string    `json:"company"`
	Phone     string    `json:"phone"`
	Wechat    string    `json:"wechat"`
	Email     string    `json:"email"`
	Source    string    `json:"source"`
	Tags      []string  `json:"tags"`
	Note      string    `json:"note"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Input struct {
	Name    string
	Company string
	Phone   string
	Wechat  string
	Email   string
	Source  string
	Tags    []string
	Note    string
}

type UpdateInput struct {
	Name    *string
	Company *string
	Phone   *string
	Wechat  *string
	Email   *string
	Source  *string
	Tags    *[]string
	Note    *string
}

type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

const columns = `id, name, company, phone, wechat, email, source, tags, note, version, created_at, updated_at`

func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in Input) (*Client, error) {
	clean, err := normalize(in)
	if err != nil {
		return nil, err
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Client, error) {
		row := tx.QueryRow(ctx, `
			INSERT INTO clients (tenant_id, name, company, phone, wechat, email, source, tags, note)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			RETURNING `+columns,
			tenantID, clean.Name, clean.Company, clean.Phone, clean.Wechat, clean.Email, clean.Source, clean.Tags, clean.Note)
		c, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("insert client: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "client.created", "client", c.ID.String(), ip,
			map[string]any{"name": c.Name}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "client", c.ID.String(), "client.created",
			map[string]any{"name": c.Name}); err != nil {
			return nil, err
		}
		return c, nil
	})
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, q string, limit, offset int) ([]Client, int, error) {
	q = strings.TrimSpace(q)
	var (
		items []Client
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM clients
			WHERE tenant_id = $1 AND ($2::text = '' OR name ILIKE '%' || $2 || '%'
				OR company ILIKE '%' || $2 || '%' OR phone LIKE '%' || $2 || '%'
				OR email ILIKE '%' || $2 || '%')`,
			tenantID, q).Scan(&total); err != nil {
			return fmt.Errorf("count clients: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT `+columns+` FROM clients
			WHERE tenant_id = $1 AND ($2::text = '' OR name ILIKE '%' || $2 || '%'
				OR company ILIKE '%' || $2 || '%' OR phone LIKE '%' || $2 || '%'
				OR email ILIKE '%' || $2 || '%')
			ORDER BY created_at DESC
			LIMIT $3 OFFSET $4`, tenantID, q, limit, offset)
		if err != nil {
			return fmt.Errorf("list clients: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scan(rows)
			if err != nil {
				return fmt.Errorf("scan client: %w", err)
			}
			items = append(items, *c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Client{}
	}
	return items, total, nil
}

func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (*Client, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Client, error) {
		c, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM clients WHERE id = $1 AND tenant_id = $2`, id, tenantID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("get client: %w", err)
		}
		return c, nil
	})
}

func (s *Service) Update(ctx context.Context, tenantID, actor, id uuid.UUID, ip string, expectedVersion int, in UpdateInput) (*Client, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Client, error) {
		var version int
		err := tx.QueryRow(ctx, `SELECT version FROM clients WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			id, tenantID).Scan(&version)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("lock client: %w", err)
		}
		if version != expectedVersion {
			return nil, ErrVersionConflict
		}

		var tags []string
		if in.Tags != nil {
			var err error
			if tags, err = normalizeTags(*in.Tags); err != nil {
				return nil, err
			}
		}
		if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
			return nil, fmt.Errorf("%w: name must not be empty", ErrInvalidInput)
		}
		if in.Email != nil {
			if err := validateEmail(strings.TrimSpace(*in.Email)); err != nil {
				return nil, err
			}
			trimmed := strings.TrimSpace(*in.Email)
			in.Email = &trimmed
		}

		row := tx.QueryRow(ctx, `
			UPDATE clients SET
				name = COALESCE($4, name), company = COALESCE($5, company),
				phone = COALESCE($6, phone), wechat = COALESCE($7, wechat),
				email = COALESCE($8, email),
				source = COALESCE($9, source), tags = COALESCE($10, tags),
				note = COALESCE($11, note), version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND version = $3
			RETURNING `+columns,
			id, tenantID, expectedVersion, in.Name, in.Company, in.Phone, in.Wechat, in.Email, in.Source, tags, in.Note)
		c, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("update client: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "client.updated", "client", c.ID.String(), ip,
			map[string]any{"version": c.Version}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "client", c.ID.String(), "client.updated",
			map[string]any{"version": c.Version}); err != nil {
			return nil, err
		}
		return c, nil
	})
}

func normalize(in Input) (Input, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Company = strings.TrimSpace(in.Company)
	in.Phone = strings.TrimSpace(in.Phone)
	in.Wechat = strings.TrimSpace(in.Wechat)
	in.Email = strings.TrimSpace(in.Email)
	in.Source = strings.TrimSpace(in.Source)
	in.Note = strings.TrimSpace(in.Note)
	if in.Name == "" {
		return Input{}, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if err := validateEmail(in.Email); err != nil {
		return Input{}, err
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return Input{}, err
	}
	in.Tags = tags
	return in, nil
}

// validateEmail accepts a bare address only; display names belong in the client
// name field. An empty address is valid because not every client has one until
// proof invitations are sent.
func validateEmail(email string) error {
	if email == "" {
		return nil
	}
	if len(email) > 254 {
		return fmt.Errorf("%w: email is too long", ErrInvalidInput)
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return fmt.Errorf("%w: email is not a valid address", ErrInvalidInput)
	}
	return nil
}

func normalizeTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len([]rune(tag)) > 32 {
			return nil, fmt.Errorf("%w: tag must be at most 32 characters", ErrInvalidInput)
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	if len(out) > 20 {
		return nil, fmt.Errorf("%w: at most 20 tags", ErrInvalidInput)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (*Client, error) {
	var c Client
	if err := row.Scan(&c.ID, &c.Name, &c.Company, &c.Phone, &c.Wechat, &c.Email,
		&c.Source, &c.Tags, &c.Note, &c.Version, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if c.Tags == nil {
		c.Tags = []string{}
	}
	return &c, nil
}
