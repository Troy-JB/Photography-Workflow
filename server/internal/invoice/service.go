package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/payment"
	"photography-workflow/server/internal/storage"
)

var (
	ErrNotFound          = errors.New("invoice not found")
	ErrRedFlushNotFound  = errors.New("invoice red flush not found")
	ErrPaymentNotFound   = errors.New("payment not found")
	ErrVersionConflict   = errors.New("invoice version conflict")
	ErrIllegalState      = errors.New("illegal invoice state")
	ErrInvalidInput      = errors.New("invalid invoice input")
	ErrActiveInvoice     = errors.New("payment already has an active invoice")
	ErrInvoiceNoConflict = errors.New("invoice number already used")
	ErrFileMismatch      = errors.New("invoice file does not match the recorded hash")
	ErrProvider          = errors.New("invoice provider error")
)

const (
	maxInvoiceFileBytes = 20 << 20
	fileURLTTL          = 15 * time.Minute
)

const invoiceColumns = `id, project_id, payment_id, type, status, title, tax_no, email,
	address, phone, bank_name, bank_account, amount_cents, currency, remark,
	invoice_no, invoice_code, file_key, file_hash, file_size,
	requested_by, reviewed_by, reviewed_at, reject_reason,
	issued_at, sent_at, voided_at, void_reason, version, created_at, updated_at`

const redFlushColumns = `id, invoice_id, refund_id, amount_cents, status,
	red_flush_no, red_flush_code, file_key, file_hash, file_size, reason,
	issued_by, issued_at, created_at, updated_at`

type Invoice struct {
	ID           uuid.UUID  `json:"id"`
	ProjectID    uuid.UUID  `json:"project_id"`
	PaymentID    uuid.UUID  `json:"payment_id"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	Title        string     `json:"title"`
	TaxNo        string     `json:"tax_no"`
	Email        string     `json:"email"`
	Address      string     `json:"address"`
	Phone        string     `json:"phone"`
	BankName     string     `json:"bank_name"`
	BankAccount  string     `json:"bank_account"`
	AmountCents  int64      `json:"amount_cents"`
	Currency     string     `json:"currency"`
	Remark       string     `json:"remark"`
	InvoiceNo    string     `json:"invoice_no,omitempty"`
	InvoiceCode  string     `json:"invoice_code,omitempty"`
	FileKey      string     `json:"file_key,omitempty"`
	FileHash     string     `json:"file_hash,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
	RequestedBy  *uuid.UUID `json:"requested_by,omitempty"`
	ReviewedBy   *uuid.UUID `json:"reviewed_by,omitempty"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
	RejectReason string     `json:"reject_reason,omitempty"`
	IssuedAt     *time.Time `json:"issued_at,omitempty"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
	VoidedAt     *time.Time `json:"voided_at,omitempty"`
	VoidReason   string     `json:"void_reason,omitempty"`
	Version      int        `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	RedFlushes   []RedFlush `json:"red_flushes,omitempty"`
}

type RedFlush struct {
	ID           uuid.UUID  `json:"id"`
	InvoiceID    uuid.UUID  `json:"invoice_id"`
	RefundID     uuid.UUID  `json:"refund_id"`
	AmountCents  int64      `json:"amount_cents"`
	Status       string     `json:"status"`
	RedFlushNo   string     `json:"red_flush_no,omitempty"`
	RedFlushCode string     `json:"red_flush_code,omitempty"`
	FileKey      string     `json:"file_key,omitempty"`
	FileHash     string     `json:"file_hash,omitempty"`
	FileSize     int64      `json:"file_size,omitempty"`
	Reason       string     `json:"reason"`
	IssuedBy     *uuid.UUID `json:"issued_by,omitempty"`
	IssuedAt     *time.Time `json:"issued_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type FileUpload struct {
	ObjectKey string    `json:"object_key"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Service struct {
	pool     *pgxpool.Pool
	store    storage.Storage
	provider Provider
	alerts   *alert.Service
	log      *slog.Logger
}

func NewService(pool *pgxpool.Pool, store storage.Storage, provider Provider, alerts *alert.Service, log *slog.Logger) *Service {
	return &Service{pool: pool, store: store, provider: provider, alerts: alerts, log: log}
}

type CreateInput struct {
	PaymentID   uuid.UUID
	Type        string
	Title       string
	TaxNo       string
	Email       string
	Address     string
	Phone       string
	BankName    string
	BankAccount string
	Remark      string
}

// Create ties the invoice to a settled payment. The amount is taken from the
// payment row so a client can never invoice a different amount.
func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateInput) (*Invoice, error) {
	if in.PaymentID == uuid.Nil {
		return nil, fmt.Errorf("%w: payment_id is required", ErrInvalidInput)
	}
	typ := strings.ToLower(strings.TrimSpace(in.Type))
	if typ == "" {
		typ = TypeNormal
	}
	if !ValidType(typ) {
		return nil, fmt.Errorf("%w: type must be normal or special", ErrInvalidInput)
	}
	title := strings.TrimSpace(in.Title)
	taxNo := strings.TrimSpace(in.TaxNo)
	email := strings.TrimSpace(in.Email)
	address := strings.TrimSpace(in.Address)
	phone := strings.TrimSpace(in.Phone)
	bankName := strings.TrimSpace(in.BankName)
	bankAccount := strings.TrimSpace(in.BankAccount)
	remark := strings.TrimSpace(in.Remark)
	switch {
	case title == "" || len(title) > 200:
		return nil, fmt.Errorf("%w: title is required and must be at most 200 characters", ErrInvalidInput)
	case len(taxNo) > 40, len(email) > 200, len(address) > 300, len(phone) > 40:
		return nil, fmt.Errorf("%w: invoice fields are too long", ErrInvalidInput)
	case len(bankName) > 100 || len(bankAccount) > 100:
		return nil, fmt.Errorf("%w: bank fields are too long", ErrInvalidInput)
	case len(remark) > 500:
		return nil, fmt.Errorf("%w: remark is too long", ErrInvalidInput)
	case typ == TypeSpecial && taxNo == "":
		return nil, fmt.Errorf("%w: a special invoice requires the buyer tax number", ErrInvalidInput)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Invoice, error) {
		var (
			projectID uuid.UUID
			amount    int64
			currency  string
		)
		if err := tx.QueryRow(ctx, `
			SELECT project_id, amount_cents, currency FROM payments
			WHERE id = $1 AND tenant_id = $2`, in.PaymentID, tenantID).Scan(&projectID, &amount, &currency); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrPaymentNotFound
			}
			return nil, fmt.Errorf("load payment for invoice: %w", err)
		}
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM invoices
				WHERE tenant_id = $1 AND payment_id = $2 AND status NOT IN ('voided','rejected'))`,
			tenantID, in.PaymentID).Scan(&active); err != nil {
			return nil, fmt.Errorf("check active invoice: %w", err)
		}
		if active {
			return nil, ErrActiveInvoice
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO invoices (tenant_id, project_id, payment_id, type, status, title, tax_no,
				email, address, phone, bank_name, bank_account, amount_cents, currency, remark, requested_by)
			VALUES ($1,$2,$3,$4,'requested',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			RETURNING `+invoiceColumns,
			tenantID, projectID, in.PaymentID, typ, title, taxNo, email, address, phone,
			bankName, bankAccount, amount, currency, remark, actor)
		inv, err := scanInvoice(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrActiveInvoice
			}
			return nil, fmt.Errorf("create invoice: %w", err)
		}
		detail := map[string]any{
			"project_id": inv.ProjectID, "payment_id": inv.PaymentID, "type": inv.Type,
			"amount_cents": inv.AmountCents, "currency": inv.Currency,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "invoice.requested", "invoice", inv.ID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "invoice", inv.ID.String(), "invoice.requested", detail); err != nil {
			return nil, err
		}
		return inv, nil
	})
}

func (s *Service) Approve(ctx context.Context, tenantID, actor, invoiceID uuid.UUID, ip string, expectedVersion int) (*Invoice, error) {
	return s.review(ctx, tenantID, actor, invoiceID, ip, expectedVersion, "")
}

func (s *Service) Reject(ctx context.Context, tenantID, actor, invoiceID uuid.UUID, ip string, expectedVersion int, reason string) (*Invoice, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return nil, fmt.Errorf("%w: a rejection reason is required and must be at most 500 characters", ErrInvalidInput)
	}
	return s.review(ctx, tenantID, actor, invoiceID, ip, expectedVersion, reason)
}

func (s *Service) review(ctx context.Context, tenantID, actor, invoiceID uuid.UUID, ip string, expectedVersion int, rejectReason string) (*Invoice, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Invoice, error) {
		inv, err := lockInvoice(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return nil, err
		}
		if inv.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		status, action, event := StatusApproved, "invoice.approved", "invoice.approved"
		if rejectReason != "" {
			if !CanReject(inv.Status) {
				return nil, fmt.Errorf("%w: cannot reject a %s invoice", ErrIllegalState, inv.Status)
			}
			status, action, event = StatusRejected, "invoice.rejected", "invoice.rejected"
		} else if !CanApprove(inv.Status) {
			return nil, fmt.Errorf("%w: cannot approve a %s invoice", ErrIllegalState, inv.Status)
		}
		row := tx.QueryRow(ctx, `
			UPDATE invoices SET status = $3, reviewed_by = $4, reviewed_at = now(),
				reject_reason = $5, version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+invoiceColumns,
			invoiceID, tenantID, status, actor, rejectReason)
		updated, err := scanInvoice(row)
		if err != nil {
			return nil, fmt.Errorf("review invoice: %w", err)
		}
		detail := map[string]any{
			"project_id": updated.ProjectID, "payment_id": updated.PaymentID,
			"status": updated.Status,
		}
		if rejectReason != "" {
			detail["reason"] = rejectReason
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, action, "invoice", invoiceID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "invoice", invoiceID.String(), event, detail); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

func (s *Service) Void(ctx context.Context, tenantID, actor, invoiceID uuid.UUID, ip string, expectedVersion int, reason string) (*Invoice, error) {
	reason = strings.TrimSpace(reason)
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	if reason == "" || len(reason) > 500 {
		return nil, fmt.Errorf("%w: a void reason is required and must be at most 500 characters", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Invoice, error) {
		inv, err := lockInvoice(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return nil, err
		}
		if inv.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanVoid(inv.Status) {
			return nil, fmt.Errorf("%w: cannot void a %s invoice; issued invoices are corrected by a red flush", ErrIllegalState, inv.Status)
		}
		row := tx.QueryRow(ctx, `
			UPDATE invoices SET status = 'voided', void_reason = $3, voided_at = now(),
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+invoiceColumns, invoiceID, tenantID, reason)
		updated, err := scanInvoice(row)
		if err != nil {
			return nil, fmt.Errorf("void invoice: %w", err)
		}
		detail := map[string]any{"project_id": updated.ProjectID, "reason": reason}
		if err := audit.Write(ctx, tx, tenantID, &actor, "invoice.voided", "invoice", invoiceID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "invoice", invoiceID.String(), "invoice.voided", detail); err != nil {
			return nil, err
		}
		return updated, nil
	})
}

// FileURL reserves the object key and returns a presigned PUT. Invoice files
// only exist under tenants/{tenant}/invoices/{invoice}/, which issue validates
// before trusting the uploaded object.
func (s *Service) FileURL(ctx context.Context, tenantID, invoiceID uuid.UUID, redFlushID *uuid.UUID) (*FileUpload, error) {
	var key string
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		inv, err := loadInvoice(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return err
		}
		if redFlushID != nil {
			rf, err := loadRedFlush(ctx, tx, tenantID, invoiceID, *redFlushID)
			if err != nil {
				return err
			}
			if rf.Status != RedFlushPending {
				return fmt.Errorf("%w: red flush is %s", ErrIllegalState, rf.Status)
			}
			key = fmt.Sprintf("tenants/%s/invoices/%s/red-flush-%s.pdf", tenantID, invoiceID, rf.ID)
			return nil
		}
		if inv.Status != StatusApproved {
			return fmt.Errorf("%w: invoice must be approved before its file is uploaded, got %s", ErrIllegalState, inv.Status)
		}
		key = fmt.Sprintf("tenants/%s/invoices/%s/v%d.pdf", tenantID, invoiceID, inv.Version+1)
		return nil
	})
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(fileURLTTL)
	url, err := s.store.PresignPut(ctx, key, fileURLTTL)
	if err != nil {
		return nil, fmt.Errorf("presign invoice file: %w", err)
	}
	return &FileUpload{ObjectKey: key, URL: url, ExpiresAt: expiresAt}, nil
}

type IssueInput struct {
	ExpectedVersion int
	InvoiceNo       string
	InvoiceCode     string
	FileKey         string
	FileHash        string
}

func (s *Service) Issue(ctx context.Context, tenantID, actor, invoiceID uuid.UUID, ip string, in IssueInput) (*Invoice, error) {
	if in.ExpectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Invoice, error) {
		inv, err := lockInvoice(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return nil, err
		}
		if inv.Version != in.ExpectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanIssue(inv.Status) {
			return nil, fmt.Errorf("%w: cannot issue a %s invoice", ErrIllegalState, inv.Status)
		}
		fileKey, fileHash, err := validateFile(in.FileKey, in.FileHash)
		if err != nil {
			return nil, err
		}
		if err := s.checkFileKey(tenantID, invoiceID, fileKey); err != nil {
			return nil, err
		}
		size, err := s.verifyFile(ctx, fileKey, fileHash)
		if err != nil {
			return nil, err
		}
		result, err := s.provider.Issue(ctx, IssueRequest{
			InvoiceID: invoiceID, InvoiceNo: in.InvoiceNo, InvoiceCode: in.InvoiceCode,
			Title: inv.Title, TaxNo: inv.TaxNo, AmountCents: inv.AmountCents, Currency: inv.Currency,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProvider, err)
		}
		row := tx.QueryRow(ctx, `
			UPDATE invoices SET status = 'issued', invoice_no = $3, invoice_code = $4,
				file_key = $5, file_hash = $6, file_size = $7, issued_at = now(),
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+invoiceColumns,
			invoiceID, tenantID, result.InvoiceNo, result.InvoiceCode, fileKey, fileHash, size)
		issued, err := scanInvoice(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrInvoiceNoConflict
			}
			return nil, fmt.Errorf("issue invoice: %w", err)
		}
		// A refund may have settled before the invoice was issued; queue the red
		// flush now so an issued invoice never stays over-invoiced.
		if err := s.ensureRedFlushes(ctx, tx, tenantID, issued, &actor, ip); err != nil {
			return nil, err
		}
		detail := map[string]any{
			"project_id": issued.ProjectID, "payment_id": issued.PaymentID,
			"invoice_no": issued.InvoiceNo, "amount_cents": issued.AmountCents,
			"file_hash": issued.FileHash, "provider": s.provider.Name(),
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "invoice.issued", "invoice", invoiceID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "invoice", invoiceID.String(), "invoice.issued", detail); err != nil {
			return nil, err
		}
		return issued, nil
	})
}

func (s *Service) Send(ctx context.Context, tenantID, actor, invoiceID uuid.UUID, ip string, expectedVersion int) (*Invoice, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Invoice, error) {
		inv, err := lockInvoice(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return nil, err
		}
		if inv.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if inv.Status == StatusSent {
			return inv, nil
		}
		if !CanSend(inv.Status) {
			return nil, fmt.Errorf("%w: cannot send a %s invoice", ErrIllegalState, inv.Status)
		}
		if inv.Email == "" {
			return nil, fmt.Errorf("%w: an email address is required to send the invoice", ErrInvalidInput)
		}
		row := tx.QueryRow(ctx, `
			UPDATE invoices SET status = 'sent', sent_at = now(),
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+invoiceColumns, invoiceID, tenantID)
		sent, err := scanInvoice(row)
		if err != nil {
			return nil, fmt.Errorf("send invoice: %w", err)
		}
		detail := map[string]any{"project_id": sent.ProjectID, "email": sent.Email, "invoice_no": sent.InvoiceNo}
		if err := audit.Write(ctx, tx, tenantID, &actor, "invoice.sent", "invoice", invoiceID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "invoice", invoiceID.String(), "invoice.sent", detail); err != nil {
			return nil, err
		}
		return sent, nil
	})
}

type RedFlushIssueInput struct {
	RedFlushNo   string
	RedFlushCode string
	FileKey      string
	FileHash     string
}

func (s *Service) IssueRedFlush(ctx context.Context, tenantID, actor, invoiceID, redFlushID uuid.UUID, ip string, in RedFlushIssueInput) (*RedFlush, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*RedFlush, error) {
		inv, err := lockInvoice(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return nil, err
		}
		rf, err := lockRedFlush(ctx, tx, tenantID, invoiceID, redFlushID)
		if err != nil {
			return nil, err
		}
		if rf.Status != RedFlushPending {
			return nil, fmt.Errorf("%w: red flush is already %s", ErrIllegalState, rf.Status)
		}
		fileKey, fileHash, err := validateFile(in.FileKey, in.FileHash)
		if err != nil {
			return nil, err
		}
		if err := s.checkFileKey(tenantID, invoiceID, fileKey); err != nil {
			return nil, err
		}
		size, err := s.verifyFile(ctx, fileKey, fileHash)
		if err != nil {
			return nil, err
		}
		result, err := s.provider.RedFlush(ctx, RedFlushRequest{
			InvoiceID: invoiceID, RedFlushID: redFlushID, RedFlushNo: in.RedFlushNo,
			RedFlushCode: in.RedFlushCode, AmountCents: rf.AmountCents,
			Currency: inv.Currency, Reason: rf.Reason,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProvider, err)
		}
		row := tx.QueryRow(ctx, `
			UPDATE invoice_red_flushes SET status = 'issued', red_flush_no = $3,
				red_flush_code = $4, file_key = $5, file_hash = $6, file_size = $7,
				issued_by = $8, issued_at = now(), updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+redFlushColumns,
			redFlushID, tenantID, result.RedFlushNo, result.RedFlushCode, fileKey, fileHash, size, actor)
		issued, err := scanRedFlush(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrInvoiceNoConflict
			}
			return nil, fmt.Errorf("issue red flush: %w", err)
		}
		detail := map[string]any{
			"project_id": inv.ProjectID, "invoice_id": invoiceID, "refund_id": rf.RefundID,
			"red_flush_no": issued.RedFlushNo, "amount_cents": issued.AmountCents,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "invoice.red_flush_issued", "invoice_red_flush", redFlushID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "invoice_red_flush", redFlushID.String(), "invoice.red_flush_issued", detail); err != nil {
			return nil, err
		}
		return issued, nil
	})
}

func (s *Service) Get(ctx context.Context, tenantID, invoiceID uuid.UUID) (*Invoice, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Invoice, error) {
		inv, err := loadInvoice(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return nil, err
		}
		redFlushes, err := listRedFlushes(ctx, tx, tenantID, invoiceID)
		if err != nil {
			return nil, err
		}
		inv.RedFlushes = redFlushes
		return inv, nil
	})
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, projectID, paymentID *uuid.UUID, status string, limit, offset int) ([]Invoice, int, error) {
	if status != "" && !ValidStatus(status) {
		return nil, 0, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
	}
	var (
		items []Invoice
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const filter = `tenant_id = $1 AND ($2::uuid IS NULL OR project_id = $2)
			AND ($3::uuid IS NULL OR payment_id = $3) AND ($4::text = '' OR status = $4::text)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE `+filter,
			tenantID, projectID, paymentID, status).Scan(&total); err != nil {
			return fmt.Errorf("count invoices: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+invoiceColumns+` FROM invoices WHERE `+filter+`
			ORDER BY created_at DESC LIMIT $5 OFFSET $6`, tenantID, projectID, paymentID, status, limit, offset)
		if err != nil {
			return fmt.Errorf("list invoices: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvoice(rows)
			if err != nil {
				return err
			}
			items = append(items, *inv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Invoice{}
	}
	return items, total, nil
}

func (s *Service) ListRedFlushes(ctx context.Context, tenantID, invoiceID uuid.UUID) ([]RedFlush, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) ([]RedFlush, error) {
		if _, err := loadInvoice(ctx, tx, tenantID, invoiceID); err != nil {
			return nil, err
		}
		return listRedFlushes(ctx, tx, tenantID, invoiceID)
	})
}

// RefundSettled implements payment.RefundHook. A refund against an issued
// invoice queues a red-flush work item; the refund transaction fails if the
// work item cannot be recorded, so books never silently drift.
func (s *Service) RefundSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p *payment.Payment, refund *payment.Refund) error {
	if p == nil || refund == nil {
		return nil
	}
	inv, err := scanInvoice(tx.QueryRow(ctx, `
		SELECT `+invoiceColumns+` FROM invoices
		WHERE tenant_id = $1 AND payment_id = $2 AND status IN ('issued','sent')`,
		tenantID, p.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load invoice for refund: %w", err)
	}
	return s.ensureRedFlushes(ctx, tx, tenantID, inv, nil, "")
}

// ensureRedFlushes queues one pending red flush per succeeded refund until the
// invoice amount is covered. It is idempotent: refund_id is unique, and the
// remaining amount is derived from the rows already queued.
func (s *Service) ensureRedFlushes(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, inv *Invoice, actor *uuid.UUID, ip string) error {
	queued := make(map[uuid.UUID]int64)
	var used int64
	rows, err := tx.Query(ctx, `
		SELECT refund_id, amount_cents FROM invoice_red_flushes
		WHERE tenant_id = $1 AND invoice_id = $2`, tenantID, inv.ID)
	if err != nil {
		return fmt.Errorf("load red flushes: %w", err)
	}
	for rows.Next() {
		var refundID uuid.UUID
		var amount int64
		if err := rows.Scan(&refundID, &amount); err != nil {
			rows.Close()
			return fmt.Errorf("scan red flush: %w", err)
		}
		queued[refundID] = amount
		used += amount
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	remaining := inv.AmountCents - used
	if remaining <= 0 {
		return nil
	}
	type settledRefund struct {
		id     uuid.UUID
		amount int64
		reason string
	}
	var refunds []settledRefund
	rows, err = tx.Query(ctx, `
		SELECT id, amount_cents, reason FROM refunds
		WHERE tenant_id = $1 AND payment_id = $2 AND status = 'succeeded'
		ORDER BY created_at`, tenantID, inv.PaymentID)
	if err != nil {
		return fmt.Errorf("load settled refunds: %w", err)
	}
	for rows.Next() {
		var r settledRefund
		if err := rows.Scan(&r.id, &r.amount, &r.reason); err != nil {
			rows.Close()
			return fmt.Errorf("scan settled refund: %w", err)
		}
		refunds = append(refunds, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, refund := range refunds {
		if remaining <= 0 {
			break
		}
		if _, ok := queued[refund.id]; ok {
			continue
		}
		amount := refund.amount
		if amount > remaining {
			amount = remaining
		}
		var redFlushID uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO invoice_red_flushes (tenant_id, invoice_id, refund_id, amount_cents, status, reason)
			VALUES ($1,$2,$3,$4,'pending',$5)
			ON CONFLICT (refund_id) DO NOTHING
			RETURNING id`,
			tenantID, inv.ID, refund.id, amount, refund.reason).Scan(&redFlushID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("queue red flush: %w", err)
		}
		remaining -= amount
		detail := map[string]any{
			"project_id": inv.ProjectID, "invoice_id": inv.ID, "refund_id": refund.id,
			"amount_cents": amount, "reason": refund.reason,
		}
		if err := audit.Write(ctx, tx, tenantID, actor, "invoice.red_flush_required", "invoice_red_flush", redFlushID.String(), ip, detail); err != nil {
			return err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "invoice_red_flush", redFlushID.String(), "invoice.red_flush_required", detail); err != nil {
			return err
		}
		if s.alerts != nil {
			amountCents := amount
			if _, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
				ProjectID:   &inv.ProjectID,
				SubjectType: "invoice_red_flush",
				SubjectID:   redFlushID.String(),
				Code:        "invoice_red_flush_required",
				Severity:    alert.SeverityInfo,
				DedupeKey:   "invoice_red_flush:" + inv.ID.String() + ":" + refund.id.String(),
				Title:       "Refund settled against an issued invoice: red flush required",
				Detail:      detail,
				AmountCents: &amountCents,
			}); err != nil {
				s.log.Error("raise red flush alert", "invoice_id", inv.ID, "refund_id", refund.id, "err", err)
			}
		}
	}
	return nil
}

func (s *Service) checkFileKey(tenantID, invoiceID uuid.UUID, key string) error {
	prefix := fmt.Sprintf("tenants/%s/invoices/%s/", tenantID, invoiceID)
	if !strings.HasPrefix(key, prefix) {
		return fmt.Errorf("%w: file_key must live under %s", ErrInvalidInput, prefix)
	}
	return nil
}

func (s *Service) verifyFile(ctx context.Context, key, wantHash string) (int64, error) {
	size, err := s.store.Stat(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, fmt.Errorf("%w: the invoice file was not uploaded", ErrInvalidInput)
	}
	if err != nil {
		return 0, fmt.Errorf("stat invoice file: %w", err)
	}
	if size <= 0 || size > maxInvoiceFileBytes {
		return 0, fmt.Errorf("%w: invoice file must be between 1 byte and %d bytes", ErrInvalidInput, maxInvoiceFileBytes)
	}
	rc, err := s.store.Open(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("open invoice file: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, maxInvoiceFileBytes+1))
	if err != nil {
		return 0, fmt.Errorf("read invoice file: %w", err)
	}
	if n > maxInvoiceFileBytes {
		return 0, fmt.Errorf("%w: invoice file is too large", ErrInvalidInput)
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), wantHash) {
		return 0, ErrFileMismatch
	}
	return size, nil
}

func validateFile(key, hash string) (string, string, error) {
	key = strings.TrimSpace(key)
	hash = strings.ToLower(strings.TrimSpace(hash))
	if key == "" {
		return "", "", fmt.Errorf("%w: file_key is required", ErrInvalidInput)
	}
	if !validSHA256(hash) {
		return "", "", fmt.Errorf("%w: file_hash must be a sha256 hex digest", ErrInvalidInput)
	}
	return key, hash, nil
}

func validSHA256(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

func loadInvoice(ctx context.Context, tx pgx.Tx, tenantID, invoiceID uuid.UUID) (*Invoice, error) {
	inv, err := scanInvoice(tx.QueryRow(ctx, `
		SELECT `+invoiceColumns+` FROM invoices WHERE id = $1 AND tenant_id = $2`,
		invoiceID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load invoice: %w", err)
	}
	return inv, nil
}

func lockInvoice(ctx context.Context, tx pgx.Tx, tenantID, invoiceID uuid.UUID) (*Invoice, error) {
	inv, err := scanInvoice(tx.QueryRow(ctx, `
		SELECT `+invoiceColumns+` FROM invoices WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		invoiceID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock invoice: %w", err)
	}
	return inv, nil
}

func loadRedFlush(ctx context.Context, tx pgx.Tx, tenantID, invoiceID, redFlushID uuid.UUID) (*RedFlush, error) {
	rf, err := scanRedFlush(tx.QueryRow(ctx, `
		SELECT `+redFlushColumns+` FROM invoice_red_flushes
		WHERE id = $1 AND tenant_id = $2 AND invoice_id = $3`,
		redFlushID, tenantID, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRedFlushNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load red flush: %w", err)
	}
	return rf, nil
}

func lockRedFlush(ctx context.Context, tx pgx.Tx, tenantID, invoiceID, redFlushID uuid.UUID) (*RedFlush, error) {
	rf, err := scanRedFlush(tx.QueryRow(ctx, `
		SELECT `+redFlushColumns+` FROM invoice_red_flushes
		WHERE id = $1 AND tenant_id = $2 AND invoice_id = $3 FOR UPDATE`,
		redFlushID, tenantID, invoiceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRedFlushNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock red flush: %w", err)
	}
	return rf, nil
}

func listRedFlushes(ctx context.Context, tx pgx.Tx, tenantID, invoiceID uuid.UUID) ([]RedFlush, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+redFlushColumns+` FROM invoice_red_flushes
		WHERE tenant_id = $1 AND invoice_id = $2 ORDER BY created_at`, tenantID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("list red flushes: %w", err)
	}
	defer rows.Close()
	items := []RedFlush{}
	for rows.Next() {
		rf, err := scanRedFlush(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *rf)
	}
	return items, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanInvoice(row rowScanner) (*Invoice, error) {
	var inv Invoice
	if err := row.Scan(&inv.ID, &inv.ProjectID, &inv.PaymentID, &inv.Type, &inv.Status,
		&inv.Title, &inv.TaxNo, &inv.Email, &inv.Address, &inv.Phone, &inv.BankName,
		&inv.BankAccount, &inv.AmountCents, &inv.Currency, &inv.Remark, &inv.InvoiceNo,
		&inv.InvoiceCode, &inv.FileKey, &inv.FileHash, &inv.FileSize, &inv.RequestedBy,
		&inv.ReviewedBy, &inv.ReviewedAt, &inv.RejectReason, &inv.IssuedAt, &inv.SentAt,
		&inv.VoidedAt, &inv.VoidReason, &inv.Version, &inv.CreatedAt, &inv.UpdatedAt); err != nil {
		return nil, err
	}
	return &inv, nil
}

func scanRedFlush(row rowScanner) (*RedFlush, error) {
	var rf RedFlush
	if err := row.Scan(&rf.ID, &rf.InvoiceID, &rf.RefundID, &rf.AmountCents, &rf.Status,
		&rf.RedFlushNo, &rf.RedFlushCode, &rf.FileKey, &rf.FileHash, &rf.FileSize,
		&rf.Reason, &rf.IssuedBy, &rf.IssuedAt, &rf.CreatedAt, &rf.UpdatedAt); err != nil {
		return nil, err
	}
	return &rf, nil
}
