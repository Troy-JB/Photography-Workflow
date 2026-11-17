package delivery

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

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
	"photography-workflow/server/internal/storage"
)

// Delivery states. A delivery is created for an approved proof round, built by
// the worker and only becomes downloadable once both packages are ready.
const (
	StatusPending  = "pending"
	StatusBuilding = "building"
	StatusReady    = "ready"
	StatusFailed   = "failed"
	StatusBlocked  = "blocked"
)

// Per-item web render states.
const (
	WebPending = "pending"
	WebReady   = "ready"
	WebSkipped = "skipped"
)

// Package states.
const (
	PackagePending  = "pending"
	PackageBuilding = "building"
	PackageReady    = "ready"
	PackageFailed   = "failed"
)

// Job kinds executed by the worker.
const (
	JobBuild             = "delivery.build"
	JobRenderWeb         = "delivery.render_web"
	JobPackage           = "delivery.package"
	SubjectType          = "delivery"
	ItemSubject          = "delivery_item"
	PackSubject          = "delivery_package"
	ReadyNotice          = "delivery_ready"
	BlockedReasonPayment = "payment_required"
	WarningUnsupported   = "web_spec_unsupported_mime"
	WarningDecode        = "web_spec_decode_failed"
	WarningRenderFailed  = "web_spec_render_failed"
)

var (
	ErrNotFound             = errors.New("delivery not found")
	ErrInvalidInput         = errors.New("invalid delivery input")
	ErrRoundNotFound        = errors.New("proof round not found")
	ErrRoundNotApproved     = errors.New("proof round is not approved")
	ErrPaymentRequired      = errors.New("project balance is not paid")
	ErrProjectArchived      = errors.New("project is archived")
	ErrProjectCancelled     = errors.New("project is cancelled")
	ErrDeliveryNotReady     = errors.New("delivery is not ready")
	ErrBuildInProgress      = errors.New("a build is already in progress")
	ErrTokenInvalid         = errors.New("download token is not valid")
	ErrTokenExpired         = errors.New("download token has expired")
	ErrTokenRevoked         = errors.New("download token was revoked")
	ErrDownloadsExhausted   = errors.New("download limit reached")
	ErrTokenNotFound        = errors.New("download token not found")
	ErrDeliveryMismatch     = errors.New("session belongs to another delivery")
	ErrWebSpecUnavailable   = errors.New("web version is not available for this asset")
	ErrItemNotFound         = errors.New("delivery item not found")
	ErrPackageNotFound      = errors.New("delivery package not found")
	ErrAssetLimit           = errors.New("delivery asset limit reached")
	ErrAssetSourceMissing   = errors.New("asset source object is missing")
	ErrClientEmailMissing   = errors.New("project client has no email")
	ErrChannelUnavailable   = errors.New("email channel is not configured")
	ErrDeletionNotConfirmed = errors.New("deletion confirmation is missing")
	ErrArchiveNotReady      = errors.New("archive is not ready for deletion")
	ErrArchiveNotFound      = errors.New("archive not found")
	ErrOwnerRequired        = errors.New("only the owner can perform this action")
)

// Config carries the delivery policy knobs. Zero values are replaced by the
// documented defaults in NewService.
type Config struct {
	PublicBaseURL   string
	SessionTTL      time.Duration
	DefaultTokenTTL time.Duration
	MaxTokenTTL     time.Duration
	PresignTTL      time.Duration
	MaxDownloads    int
	MaxAssets       int
	TempDir         string
	WebEdge         int
	RequireWebSpec  bool
}

// Notifier queues the customer delivery mail inside the caller's transaction.
type Notifier interface {
	EmailAvailable() bool
	DeliveryReady(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p NotifyPayload) (uuid.UUID, error)
}

// NotifyPayload is everything the email template needs; the plaintext link is
// never stored and only lives in the message body until it is sent.
type NotifyPayload struct {
	DeliveryID   uuid.UUID
	ProjectID    uuid.UUID
	TokenID      uuid.UUID
	ProjectCode  string
	ProjectTitle string
	ItemCount    int
	Packages     []Package
	URL          string
	ExpiresAt    time.Time
	MaxDownloads int
	ClientEmail  string
}

type Service struct {
	pool     *pgxpool.Pool
	store    storage.Storage
	tokens   *auth.DeliveryTokens
	cfg      Config
	log      *slog.Logger
	notifier Notifier
	alerts   *alert.Service
}

func NewService(pool *pgxpool.Pool, store storage.Storage, tokens *auth.DeliveryTokens, cfg Config, log *slog.Logger) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * time.Minute
	}
	if cfg.DefaultTokenTTL <= 0 {
		cfg.DefaultTokenTTL = 336 * time.Hour
	}
	if cfg.MaxTokenTTL <= 0 {
		cfg.MaxTokenTTL = 720 * time.Hour
	}
	if cfg.PresignTTL <= 0 {
		cfg.PresignTTL = 5 * time.Minute
	}
	if cfg.MaxDownloads <= 0 {
		cfg.MaxDownloads = 20
	}
	if cfg.MaxAssets <= 0 {
		cfg.MaxAssets = 300
	}
	if cfg.WebEdge <= 0 {
		cfg.WebEdge = 2048
	}
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	return &Service{pool: pool, store: store, tokens: tokens, cfg: cfg, log: log}
}

func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// SetAlerts attaches the alert sink used when a build dies permanently. Build
// failures are operator-visible: the customer paid and the studio must retry
// or refund manually.
func (s *Service) SetAlerts(a *alert.Service) { s.alerts = a }

// Delivery is the JSON view of a delivery row.
type Delivery struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	ProjectID     uuid.UUID  `json:"project_id"`
	ProofRoundID  uuid.UUID  `json:"proof_round_id"`
	Status        string     `json:"status"`
	BuildVersion  int        `json:"build_version"`
	ItemCount     int        `json:"item_count"`
	TotalBytes    int64      `json:"total_bytes"`
	BlockedReason string     `json:"blocked_reason,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	ReadyAt       *time.Time `json:"ready_at,omitempty"`
	CreatedBy     *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Summary adds the project and gate state the staff UI needs.
type Summary struct {
	Delivery
	ProjectCode   string `json:"project_code"`
	ProjectTitle  string `json:"project_title"`
	RoundNo       int    `json:"round_no"`
	PaymentStatus string `json:"payment_status"`
	ProjectState  string `json:"project_state"`
}

// Package is one built ZIP. manifest is not echoed to the API; the admin UI
// reads the item list instead.
type Package struct {
	ID         uuid.UUID  `json:"id"`
	Spec       string     `json:"spec"`
	Version    int        `json:"version"`
	Status     string     `json:"status"`
	ObjectKey  string     `json:"object_key,omitempty"`
	FileName   string     `json:"file_name,omitempty"`
	SizeBytes  int64      `json:"size_bytes"`
	SHA256     string     `json:"sha256,omitempty"`
	EntryCount int        `json:"entry_count"`
	LastError  string     `json:"last_error,omitempty"`
	BuiltAt    *time.Time `json:"built_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Item is one asset snapshot with the web render state.
type Item struct {
	ID             uuid.UUID  `json:"id"`
	AssetID        uuid.UUID  `json:"asset_id"`
	Position       int        `json:"position"`
	OriginalName   string     `json:"original_name"`
	OriginalMime   string     `json:"original_mime,omitempty"`
	OriginalSize   int64      `json:"original_size"`
	OriginalSHA256 string     `json:"original_sha256,omitempty"`
	RevisionID     *uuid.UUID `json:"revision_id,omitempty"`
	RevisionNo     int        `json:"revision_no,omitempty"`
	WebName        string     `json:"web_name,omitempty"`
	WebSize        int64      `json:"web_size"`
	WebSHA256      string     `json:"web_sha256,omitempty"`
	WebWidth       int        `json:"web_width"`
	WebHeight      int        `json:"web_height"`
	WebStatus      string     `json:"web_status"`
	WebWarningCode string     `json:"web_warning_code,omitempty"`
	WebWarning     string     `json:"web_warning,omitempty"`
}

// Token is the safe representation of a download link. The plaintext and the
// hash never leave the database.
type Token struct {
	ID           uuid.UUID  `json:"id"`
	Label        string     `json:"label"`
	Recipient    string     `json:"recipient,omitempty"`
	MaxDownloads int        `json:"max_downloads"`
	Downloads    int        `json:"downloads"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP   string     `json:"last_used_ip,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// IssuedToken carries the only copy of the plaintext link.
type IssuedToken struct {
	Token
	Plaintext string `json:"token"`
	URL       string `json:"url"`
}

// Warning groups skipped web renders by reason.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

// Detail is the staff detail view: current packages, tokens and web warnings.
type Detail struct {
	Summary
	Packages   []Package `json:"packages"`
	Tokens     []Token   `json:"tokens"`
	Warnings   []Warning `json:"warnings"`
	WebReady   int       `json:"web_ready"`
	WebSkipped int       `json:"web_skipped"`
	WebPending int       `json:"web_pending"`
}

const deliveryColumns = `d.id, d.tenant_id, d.project_id, d.proof_round_id, d.status,
	d.build_version, d.item_count, d.total_bytes, d.blocked_reason, d.last_error,
	d.ready_at, d.created_by, d.created_at, d.updated_at,
	p.code, p.title, r.round_no, p.payment_status,
	CASE WHEN p.state NOT IN ('cancelled','archived')
	  AND EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
	  AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
	  THEN 'cancelled' ELSE p.state END`

const deliveryFrom = `FROM deliveries d
	JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
	JOIN proof_rounds r ON r.tenant_id = d.tenant_id AND r.id = d.proof_round_id`

func scanSummary(row scanner) (*Summary, error) {
	var d Summary
	if err := row.Scan(&d.ID, &d.TenantID, &d.ProjectID, &d.ProofRoundID, &d.Status,
		&d.BuildVersion, &d.ItemCount, &d.TotalBytes, &d.BlockedReason, &d.LastError,
		&d.ReadyAt, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt,
		&d.ProjectCode, &d.ProjectTitle, &d.RoundNo, &d.PaymentStatus, &d.ProjectState); err != nil {
		return nil, err
	}
	return &d, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func loadSummary(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (*Summary, error) {
	d, err := scanSummary(tx.QueryRow(ctx,
		`SELECT `+deliveryColumns+` `+deliveryFrom+` WHERE d.tenant_id = $1 AND d.id = $2`,
		tenantID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load delivery: %w", err)
	}
	return d, nil
}

// Create binds a delivery to an approved proof round. A second request for the
// same round returns the existing row so the staff UI can be retried safely.
func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, roundID uuid.UUID) (*Summary, bool, error) {
	type result struct {
		summary *Summary
		created bool
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*result, error) {
		var (
			roundStatus  string
			projectID    uuid.UUID
			projectState string
		)
		err := tx.QueryRow(ctx, `
			SELECT r.status, r.project_id, p.state
			FROM proof_rounds r
			JOIN projects p ON p.tenant_id = r.tenant_id AND p.id = r.project_id
			WHERE r.tenant_id = $1 AND r.id = $2`, tenantID, roundID).
			Scan(&roundStatus, &projectID, &projectState)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoundNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("load proof round: %w", err)
		}
		if err := checkProjectState(projectState); err != nil {
			return nil, err
		}
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, projectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, ErrProjectCancelled
			}
			return nil, fmt.Errorf("check delivery project state: %w", err)
		}
		if roundStatus != "approved" {
			return nil, ErrRoundNotApproved
		}

		var id uuid.UUID
		err = tx.QueryRow(ctx, `
			INSERT INTO deliveries (tenant_id, project_id, proof_round_id, status, created_by)
			VALUES ($1,$2,$3,'pending',$4)
			ON CONFLICT (tenant_id, proof_round_id) DO NOTHING
			RETURNING id`, tenantID, projectID, roundID, actor).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			if err := tx.QueryRow(ctx, `
				SELECT id FROM deliveries WHERE tenant_id = $1 AND proof_round_id = $2`,
				tenantID, roundID).Scan(&id); err != nil {
				return nil, fmt.Errorf("load existing delivery: %w", err)
			}
			summary, err := loadSummary(ctx, tx, tenantID, id)
			if err != nil {
				return nil, err
			}
			return &result{summary: summary}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("insert delivery: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "delivery.created", "delivery", id.String(), ip,
			map[string]any{"proof_round_id": roundID}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "delivery", id.String(), "delivery.created",
			map[string]any{"proof_round_id": roundID}); err != nil {
			return nil, err
		}
		summary, err := loadSummary(ctx, tx, tenantID, id)
		if err != nil {
			return nil, err
		}
		return &result{summary: summary, created: true}, nil
	})
	if err != nil {
		return nil, false, err
	}
	return res.summary, res.created, nil
}

// List returns deliveries for the staff app, newest first.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status string, limit, offset int) ([]Summary, int, error) {
	out := []Summary{}
	var total int
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		where := `d.tenant_id = $1`
		args := []any{tenantID}
		if projectID != nil {
			args = append(args, *projectID)
			where += fmt.Sprintf(" AND d.project_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			where += fmt.Sprintf(" AND d.status = $%d", len(args))
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries d WHERE `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count deliveries: %w", err)
		}
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s %s WHERE %s
			ORDER BY d.created_at DESC LIMIT $%d OFFSET $%d`,
			deliveryColumns, deliveryFrom, where, len(args)-1, len(args)), args...)
		if err != nil {
			return fmt.Errorf("list deliveries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanSummary(rows)
			if err != nil {
				return fmt.Errorf("scan delivery: %w", err)
			}
			out = append(out, *d)
		}
		return rows.Err()
	})
	return out, total, err
}

// Get returns the detail view with packages, tokens and web warnings.
func (s *Service) Get(ctx context.Context, tenantID, deliveryID uuid.UUID) (*Detail, error) {
	var detail *Detail
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		summary, err := loadSummary(ctx, tx, tenantID, deliveryID)
		if err != nil {
			return err
		}
		detail = &Detail{Summary: *summary, Packages: []Package{}, Tokens: []Token{}, Warnings: []Warning{}}

		rows, err := tx.Query(ctx, `
			SELECT id, spec, version, status, object_key, file_name, size_bytes, sha256,
			       entry_count, last_error, built_at, created_at
			FROM delivery_packages
			WHERE tenant_id = $1 AND delivery_id = $2
			ORDER BY version DESC, spec`, tenantID, deliveryID)
		if err != nil {
			return fmt.Errorf("list packages: %w", err)
		}
		for rows.Next() {
			var p Package
			if err := rows.Scan(&p.ID, &p.Spec, &p.Version, &p.Status, &p.ObjectKey, &p.FileName,
				&p.SizeBytes, &p.SHA256, &p.EntryCount, &p.LastError, &p.BuiltAt, &p.CreatedAt); err != nil {
				rows.Close()
				return fmt.Errorf("scan package: %w", err)
			}
			detail.Packages = append(detail.Packages, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if detail.Tokens, err = listTokens(ctx, tx, tenantID, deliveryID, 50); err != nil {
			return err
		}

		if err := tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE web_status = 'ready'),
			       count(*) FILTER (WHERE web_status = 'skipped'),
			       count(*) FILTER (WHERE web_status = 'pending')
			FROM delivery_items WHERE tenant_id = $1 AND delivery_id = $2`,
			tenantID, deliveryID).Scan(&detail.WebReady, &detail.WebSkipped, &detail.WebPending); err != nil {
			return fmt.Errorf("count web states: %w", err)
		}
		rows, err = tx.Query(ctx, `
			SELECT web_warning_code, coalesce(max(web_warning), ''), count(*)
			FROM delivery_items
			WHERE tenant_id = $1 AND delivery_id = $2 AND web_status = 'skipped'
			GROUP BY web_warning_code
			ORDER BY count(*) DESC`, tenantID, deliveryID)
		if err != nil {
			return fmt.Errorf("list warnings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var w Warning
			if err := rows.Scan(&w.Code, &w.Message, &w.Count); err != nil {
				return fmt.Errorf("scan warning: %w", err)
			}
			detail.Warnings = append(detail.Warnings, w)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// ListItems pages the item snapshots for the staff app.
func (s *Service) ListItems(ctx context.Context, tenantID, deliveryID uuid.UUID, webStatus string, limit, offset int) ([]Item, int, error) {
	out := []Item{}
	var total int
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := loadSummary(ctx, tx, tenantID, deliveryID); err != nil {
			return err
		}
		where := `tenant_id = $1 AND delivery_id = $2`
		args := []any{tenantID, deliveryID}
		if webStatus != "" {
			args = append(args, webStatus)
			where += fmt.Sprintf(" AND web_status = $%d", len(args))
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM delivery_items WHERE `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count items: %w", err)
		}
		args = append(args, limit, offset)
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT id, asset_id, position, original_name, original_mime, original_size,
			       original_sha256, revision_id, revision_no, web_name, web_size,
			       web_sha256, web_width, web_height, web_status, web_warning_code, web_warning
			FROM delivery_items WHERE %s ORDER BY position LIMIT $%d OFFSET $%d`,
			where, len(args)-1, len(args)), args...)
		if err != nil {
			return fmt.Errorf("list items: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	return out, total, err
}

func scanItem(row scanner) (*Item, error) {
	var it Item
	if err := row.Scan(&it.ID, &it.AssetID, &it.Position, &it.OriginalName, &it.OriginalMime,
		&it.OriginalSize, &it.OriginalSHA256, &it.RevisionID, &it.RevisionNo, &it.WebName,
		&it.WebSize, &it.WebSHA256, &it.WebWidth, &it.WebHeight, &it.WebStatus,
		&it.WebWarningCode, &it.WebWarning); err != nil {
		return nil, fmt.Errorf("scan delivery item: %w", err)
	}
	return &it, nil
}

// accessState is the gate view shared by the build, token and customer paths.
type accessState struct {
	Delivery      Delivery
	ProjectCode   string
	ProjectTitle  string
	PaymentStatus string
	ProjectState  string
	RoundNo       int
	RoundStatus   string
}

func loadAccess(ctx context.Context, tx pgx.Tx, tenantID, deliveryID uuid.UUID, forUpdate bool) (*accessState, error) {
	q := `SELECT d.id, d.tenant_id, d.project_id, d.proof_round_id, d.status,
			d.build_version, d.item_count, d.total_bytes, d.blocked_reason, d.last_error,
			d.ready_at, d.created_by, d.created_at, d.updated_at,
			p.code, p.title, p.payment_status, p.state, r.round_no, r.status
		FROM deliveries d
		JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
		JOIN proof_rounds r ON r.tenant_id = d.tenant_id AND r.id = d.proof_round_id
		WHERE d.tenant_id = $1 AND d.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF d`
	}
	var st accessState
	err := tx.QueryRow(ctx, q, tenantID, deliveryID).Scan(
		&st.Delivery.ID, &st.Delivery.TenantID, &st.Delivery.ProjectID, &st.Delivery.ProofRoundID,
		&st.Delivery.Status, &st.Delivery.BuildVersion, &st.Delivery.ItemCount, &st.Delivery.TotalBytes,
		&st.Delivery.BlockedReason, &st.Delivery.LastError, &st.Delivery.ReadyAt, &st.Delivery.CreatedBy,
		&st.Delivery.CreatedAt, &st.Delivery.UpdatedAt,
		&st.ProjectCode, &st.ProjectTitle, &st.PaymentStatus, &st.ProjectState,
		&st.RoundNo, &st.RoundStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load delivery access: %w", err)
	}
	return &st, nil
}

func checkProjectState(state string) error {
	switch state {
	case "archived":
		return ErrProjectArchived
	case "cancelled":
		return ErrProjectCancelled
	}
	return nil
}

// checkAccess applies the commercial gates. Ready and paid are separate flags
// so the build path can demand payment without requiring a finished package.
func checkAccess(st *accessState, requirePaid, requireReady bool) error {
	if err := checkProjectState(st.ProjectState); err != nil {
		return err
	}
	if requirePaid && st.PaymentStatus != "paid" {
		return ErrPaymentRequired
	}
	if st.RoundStatus != "approved" {
		return ErrRoundNotApproved
	}
	if requireReady && st.Delivery.Status != StatusReady {
		return ErrDeliveryNotReady
	}
	return nil
}

// Build starts (or restarts) the packaging pipeline. Payment is required here,
// not only at download time, so an unpaid project never consumes storage.
func (s *Service) Build(ctx context.Context, tenantID, deliveryID, actor uuid.UUID, ip string) (*Summary, error) {
	summary, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Summary, error) {
		st, err := loadAccess(ctx, tx, tenantID, deliveryID, true)
		if err != nil {
			return nil, err
		}
		if st.Delivery.Status == StatusBuilding {
			return nil, ErrBuildInProgress
		}
		if err := checkAccess(st, true, false); err != nil {
			return nil, err
		}
		version := st.Delivery.BuildVersion
		var built bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM delivery_packages WHERE tenant_id = $1 AND delivery_id = $2)`,
			tenantID, deliveryID).Scan(&built); err != nil {
			return nil, fmt.Errorf("check previous builds: %w", err)
		}
		if built || st.Delivery.Status != StatusPending {
			version++
		}
		if _, err := tx.Exec(ctx, `
			UPDATE deliveries SET status = 'building', build_version = $3, ready_at = NULL,
				blocked_reason = '', last_error = '', updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, deliveryID, version); err != nil {
			return nil, fmt.Errorf("mark delivery building: %w", err)
		}
		if _, err := jobs.Enqueue(ctx, tx, tenantID, JobBuild, SubjectType, &deliveryID,
			map[string]any{"delivery_id": deliveryID, "build_version": version}); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "delivery.build_requested", "delivery",
			deliveryID.String(), ip, map[string]any{"build_version": version}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "delivery", deliveryID.String(), "delivery.build_requested",
			map[string]any{"build_version": version}); err != nil {
			return nil, err
		}
		return loadSummary(ctx, tx, tenantID, deliveryID)
	})
	return summary, err
}

// ---- download tokens ----

type IssueTokenInput struct {
	Label         string
	Recipient     string
	ExpiresInDays int
	MaxDownloads  int
}

const tokenColumns = `id, label, recipient, max_downloads, downloads, expires_at,
	revoked_at, last_used_at, last_used_ip, created_at`

func scanToken(row scanner) (*Token, error) {
	var t Token
	if err := row.Scan(&t.ID, &t.Label, &t.Recipient, &t.MaxDownloads, &t.Downloads,
		&t.ExpiresAt, &t.RevokedAt, &t.LastUsedAt, &t.LastUsedIP, &t.CreatedAt); err != nil {
		return nil, err
	}
	return &t, nil
}

func listTokens(ctx context.Context, tx pgx.Tx, tenantID, deliveryID uuid.UUID, limit int) ([]Token, error) {
	rows, err := tx.Query(ctx, `SELECT `+tokenColumns+`
		FROM download_tokens WHERE tenant_id = $1 AND delivery_id = $2
		ORDER BY created_at DESC LIMIT $3`, tenantID, deliveryID, limit)
	if err != nil {
		return nil, fmt.Errorf("list download tokens: %w", err)
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Service) IssueToken(ctx context.Context, tenantID, deliveryID, actor uuid.UUID, ip string,
	in IssueTokenInput) (*IssuedToken, error) {
	label := strings.TrimSpace(in.Label)
	if len([]rune(label)) > 80 {
		return nil, fmt.Errorf("%w: label must be at most 80 characters", ErrInvalidInput)
	}
	recipient := strings.TrimSpace(in.Recipient)
	if len(recipient) > 200 {
		return nil, fmt.Errorf("%w: recipient must be at most 200 characters", ErrInvalidInput)
	}
	maxDays := int(s.cfg.MaxTokenTTL / (24 * time.Hour))
	if maxDays > 30 {
		maxDays = 30
	}
	if maxDays < 1 {
		maxDays = 1
	}
	days := in.ExpiresInDays
	if days == 0 {
		days = int(s.cfg.DefaultTokenTTL / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
	}
	if days < 1 || days > maxDays {
		return nil, fmt.Errorf("%w: expires_in_days must be between 1 and %d", ErrInvalidInput, maxDays)
	}
	maxDownloads := in.MaxDownloads
	if maxDownloads == 0 {
		maxDownloads = s.cfg.MaxDownloads
	}
	if maxDownloads < 1 || maxDownloads > 100 {
		return nil, fmt.Errorf("%w: max_downloads must be between 1 and 100", ErrInvalidInput)
	}

	token, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*IssuedToken, error) {
		st, err := loadAccess(ctx, tx, tenantID, deliveryID, false)
		if err != nil {
			return nil, err
		}
		if err := checkAccess(st, true, true); err != nil {
			return nil, err
		}
		return s.insertToken(ctx, tx, tenantID, deliveryID, &actor, ip, label, recipient,
			maxDownloads, time.Now().Add(time.Duration(days)*24*time.Hour))
	})
	return token, err
}

func (s *Service) insertToken(ctx context.Context, tx pgx.Tx, tenantID, deliveryID uuid.UUID,
	actor *uuid.UUID, ip, label, recipient string, maxDownloads int, expiresAt time.Time) (*IssuedToken, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate download token: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(plaintext))
	tok, err := scanToken(tx.QueryRow(ctx, `
		INSERT INTO download_tokens
			(tenant_id, delivery_id, token_hash, label, recipient, max_downloads, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+tokenColumns,
		tenantID, deliveryID, hex.EncodeToString(sum[:]), label, recipient, maxDownloads, expiresAt, actor))
	if err != nil {
		return nil, fmt.Errorf("insert download token: %w", err)
	}
	if err := audit.Write(ctx, tx, tenantID, actor, "delivery.token.created", "delivery",
		deliveryID.String(), ip, map[string]any{
			"token_id": tok.ID, "label": label, "max_downloads": maxDownloads,
		}); err != nil {
		return nil, err
	}
	return &IssuedToken{Token: *tok, Plaintext: plaintext, URL: s.deliveryURL(plaintext)}, nil
}

func (s *Service) deliveryURL(token string) string {
	if s.cfg.PublicBaseURL == "" {
		return "/#/d/" + token
	}
	return s.cfg.PublicBaseURL + "/#/d/" + token
}

// ListTokens returns the token metadata for the staff app. The plaintext and
// hash are never part of this view.
func (s *Service) ListTokens(ctx context.Context, tenantID, deliveryID uuid.UUID) ([]Token, error) {
	var out []Token
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := loadSummary(ctx, tx, tenantID, deliveryID); err != nil {
			return err
		}
		tokens, err := listTokens(ctx, tx, tenantID, deliveryID, 200)
		if err != nil {
			return err
		}
		out = tokens
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Token{}
	}
	return out, nil
}

func (s *Service) RevokeToken(ctx context.Context, tenantID, deliveryID, tokenID, actor uuid.UUID, ip string) (*Token, error) {
	token, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Token, error) {
		if _, err := loadSummary(ctx, tx, tenantID, deliveryID); err != nil {
			return nil, err
		}
		tok, err := scanToken(tx.QueryRow(ctx, `
			UPDATE download_tokens SET revoked_at = coalesce(revoked_at, now()),
				revoked_by = coalesce(revoked_by, $4)
			WHERE tenant_id = $1 AND delivery_id = $2 AND id = $3
			RETURNING `+tokenColumns, tenantID, deliveryID, tokenID, actor))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("revoke download token: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "delivery.token.revoked", "delivery",
			deliveryID.String(), ip, map[string]any{"token_id": tokenID}); err != nil {
			return nil, err
		}
		return tok, nil
	})
	return token, err
}

func (s *Service) RevokeAllTokens(ctx context.Context, tenantID, deliveryID, actor uuid.UUID, ip string) (int, error) {
	var revoked int
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := loadSummary(ctx, tx, tenantID, deliveryID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE download_tokens SET revoked_at = now(), revoked_by = $3
			WHERE tenant_id = $1 AND delivery_id = $2 AND revoked_at IS NULL`,
			tenantID, deliveryID, actor)
		if err != nil {
			return fmt.Errorf("revoke delivery tokens: %w", err)
		}
		revoked = int(tag.RowsAffected())
		return audit.Write(ctx, tx, tenantID, &actor, "delivery.token.revoked_all", "delivery",
			deliveryID.String(), ip, map[string]any{"revoked": revoked})
	})
	return revoked, err
}

// NotifyResult is returned by POST /deliveries/{id}/notify. The plaintext link
// is only in the queued email, never in the API response.
type NotifyResult struct {
	MessageID uuid.UUID `json:"message_id"`
	TokenID   uuid.UUID `json:"token_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) NotifyCustomer(ctx context.Context, tenantID, deliveryID, actor uuid.UUID, ip, recipient string) (*NotifyResult, error) {
	if s.notifier == nil || !s.notifier.EmailAvailable() {
		return nil, ErrChannelUnavailable
	}
	recipient = strings.TrimSpace(recipient)
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*NotifyResult, error) {
		st, err := loadAccess(ctx, tx, tenantID, deliveryID, false)
		if err != nil {
			return nil, err
		}
		if err := checkAccess(st, true, true); err != nil {
			return nil, err
		}
		if recipient == "" {
			recipient, err = projectClientEmail(ctx, tx, tenantID, st.Delivery.ProjectID)
			if err != nil {
				return nil, err
			}
		}
		if recipient == "" {
			return nil, ErrClientEmailMissing
		}
		pkgs, err := loadCurrentPackages(ctx, tx, tenantID, deliveryID, st.Delivery.BuildVersion)
		if err != nil {
			return nil, err
		}
		maxDays := int(s.cfg.DefaultTokenTTL / (24 * time.Hour))
		if maxDays < 1 {
			maxDays = 1
		}
		token, err := s.insertToken(ctx, tx, tenantID, deliveryID, &actor, ip, "notify", recipient,
			s.cfg.MaxDownloads, time.Now().Add(time.Duration(maxDays)*24*time.Hour))
		if err != nil {
			return nil, err
		}
		messageID, err := s.notifier.DeliveryReady(ctx, tx, tenantID, NotifyPayload{
			DeliveryID:   deliveryID,
			ProjectID:    st.Delivery.ProjectID,
			TokenID:      token.ID,
			ProjectCode:  st.ProjectCode,
			ProjectTitle: st.ProjectTitle,
			ItemCount:    st.Delivery.ItemCount,
			Packages:     pkgs,
			URL:          token.URL,
			ExpiresAt:    token.ExpiresAt,
			MaxDownloads: token.MaxDownloads,
			ClientEmail:  recipient,
		})
		if err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "delivery.notified", "delivery",
			deliveryID.String(), ip, map[string]any{"token_id": token.ID, "message_id": messageID}); err != nil {
			return nil, err
		}
		return &NotifyResult{MessageID: messageID, TokenID: token.ID, ExpiresAt: token.ExpiresAt}, nil
	})
	return res, err
}

func projectClientEmail(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) (string, error) {
	var email string
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(c.email, '')
		FROM projects p
		LEFT JOIN clients c ON c.tenant_id = p.tenant_id AND c.id = p.client_id
		WHERE p.tenant_id = $1 AND p.id = $2`, tenantID, projectID).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("load client email: %w", err)
	}
	return strings.TrimSpace(email), nil
}

func loadCurrentPackages(ctx context.Context, tx pgx.Tx, tenantID, deliveryID uuid.UUID, version int) ([]Package, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, spec, version, status, object_key, file_name, size_bytes, sha256,
		       entry_count, last_error, built_at, created_at
		FROM delivery_packages
		WHERE tenant_id = $1 AND delivery_id = $2 AND version = $3
		ORDER BY spec`, tenantID, deliveryID, version)
	if err != nil {
		return nil, fmt.Errorf("load delivery packages: %w", err)
	}
	defer rows.Close()
	out := []Package{}
	for rows.Next() {
		var p Package
		if err := rows.Scan(&p.ID, &p.Spec, &p.Version, &p.Status, &p.ObjectKey, &p.FileName,
			&p.SizeBytes, &p.SHA256, &p.EntryCount, &p.LastError, &p.BuiltAt, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan delivery package: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
