package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"photography-workflow/server/internal/esign"
	"photography-workflow/server/internal/outbox"
	projectstate "photography-workflow/server/internal/project"
)

var (
	ErrNotFound        = errors.New("contract not found")
	ErrQuoteNotFound   = errors.New("quote not found")
	ErrVersionConflict = errors.New("contract version conflict")
	ErrIllegalState    = errors.New("illegal contract state")
	ErrInvalidInput    = errors.New("invalid contract input")
	ErrActiveContract  = errors.New("project has an active contract")
	ErrUnknownProvider = errors.New("unknown e-sign provider")
	ErrProvider        = errors.New("e-sign provider error")
	ErrSendInProgress  = errors.New("contract is already being sent")
)

// submittingStaleAfter bounds how long a send may reserve a contract before a
// retry is allowed to resume it. The provider keys the task by contract id, so
// resuming cannot create a second signing task.
const submittingStaleAfter = 5 * time.Minute

type Contract struct {
	ID                 uuid.UUID       `json:"id"`
	ProjectID          uuid.UUID       `json:"project_id"`
	QuoteID            uuid.UUID       `json:"quote_id"`
	QuoteVersion       int             `json:"quote_version"`
	Status             string          `json:"status"`
	TemplateVersion    string          `json:"template_version"`
	Snapshot           json.RawMessage `json:"snapshot,omitempty"`
	DocumentKey        string          `json:"document_key,omitempty"`
	DocumentHash       string          `json:"document_hash,omitempty"`
	SignTaskID         string          `json:"sign_task_id,omitempty"`
	SignURL            string          `json:"sign_url,omitempty"`
	SignedDocumentKey  string          `json:"signed_document_key,omitempty"`
	SignedDocumentHash string          `json:"signed_document_hash,omitempty"`
	SignerName         string          `json:"signer_name"`
	SignerContact      string          `json:"signer_contact"`
	Evidence           json.RawMessage `json:"evidence,omitempty"`
	SentAt             *time.Time      `json:"sent_at,omitempty"`
	SignedAt           *time.Time      `json:"signed_at,omitempty"`
	VoidReason         string          `json:"void_reason,omitempty"`
	// IncludedRounds is how many free proof rounds the contract grants before
	// an extra_retouch change order is required.
	IncludedRounds int        `json:"included_rounds"`
	Version        int        `json:"version"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type CreateInput struct {
	ProjectID       uuid.UUID
	QuoteID         uuid.UUID
	TemplateVersion string
	SignerName      string
	SignerContact   string
	// IncludedRounds defaults to 2 when nil; the value is frozen with the
	// contract and later gates paid proof rounds.
	IncludedRounds *int
}

const columns = `id, project_id, quote_id, quote_version, status, template_version,
	snapshot, document_key, document_hash, sign_task_id, sign_url,
	signed_document_key, signed_document_hash, signer_name, signer_contact,
	evidence, sent_at, signed_at, void_reason, included_rounds, version, created_by, created_at, updated_at`

type Service struct {
	pool     *pgxpool.Pool
	provider esign.Provider
	alerts   *alert.Service
	log      *slog.Logger
}

func NewService(pool *pgxpool.Pool, provider esign.Provider, log *slog.Logger) *Service {
	return &Service{pool: pool, provider: provider, log: log}
}

// SetAlerts enables operator paging when a signing task cannot be reconciled
// with the local contract row.
func (s *Service) SetAlerts(alerts *alert.Service) { s.alerts = alerts }

// Create freezes the quote snapshot into a contract document. The contract is
// immediately "rendered": M1 has no separate template engine, so the snapshot
// hash is the integrity anchor the e-sign callback is checked against.
func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateInput) (*Contract, error) {
	if in.ProjectID == uuid.Nil || in.QuoteID == uuid.Nil {
		return nil, fmt.Errorf("%w: project_id and quote_id are required", ErrInvalidInput)
	}
	templateVersion := strings.TrimSpace(in.TemplateVersion)
	if templateVersion == "" {
		templateVersion = "v1"
	}
	if len(templateVersion) > 40 {
		return nil, fmt.Errorf("%w: template_version is too long", ErrInvalidInput)
	}
	signerName := strings.TrimSpace(in.SignerName)
	signerContact := strings.TrimSpace(in.SignerContact)
	if len(signerName) > 120 || len(signerContact) > 200 {
		return nil, fmt.Errorf("%w: signer fields are too long", ErrInvalidInput)
	}
	includedRounds := 2
	if in.IncludedRounds != nil {
		includedRounds = *in.IncludedRounds
	}
	if includedRounds < 0 || includedRounds > 50 {
		return nil, fmt.Errorf("%w: included_rounds must be between 0 and 50", ErrInvalidInput)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Contract, error) {
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, in.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrNotFound) {
				return nil, fmt.Errorf("%w: project not found", ErrInvalidInput)
			}
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: project is cancelled or archived", ErrIllegalState)
			}
			return nil, fmt.Errorf("lock project: %w", err)
		}

		var (
			quoteStatus   string
			quoteProject  uuid.UUID
			quoteVersion  int
			quoteCurrency string
			quoteTotal    int64
			quoteDeposit  int64
			quoteSnapshot json.RawMessage
		)
		if err := tx.QueryRow(ctx, `
			SELECT status, project_id, version, currency, total_cents, deposit_cents, snapshot
			FROM quotes WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			in.QuoteID, tenantID).Scan(&quoteStatus, &quoteProject, &quoteVersion, &quoteCurrency,
			&quoteTotal, &quoteDeposit, &quoteSnapshot); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrQuoteNotFound
			}
			return nil, fmt.Errorf("lock quote: %w", err)
		}
		if quoteProject != in.ProjectID {
			return nil, fmt.Errorf("%w: quote does not belong to the project", ErrInvalidInput)
		}
		if quoteStatus != "sent" && quoteStatus != "accepted" {
			return nil, fmt.Errorf("%w: contract requires a sent or accepted quote, got %s", ErrIllegalState, quoteStatus)
		}
		if len(quoteSnapshot) == 0 || string(quoteSnapshot) == "{}" || string(quoteSnapshot) == "null" {
			return nil, fmt.Errorf("%w: quote has no frozen snapshot", ErrIllegalState)
		}

		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM contracts
				WHERE tenant_id = $1 AND project_id = $2 AND status <> 'void')`,
			tenantID, in.ProjectID).Scan(&active); err != nil {
			return nil, fmt.Errorf("check active contract: %w", err)
		}
		if active {
			return nil, ErrActiveContract
		}

		if signerName == "" {
			signerName = snapshotClientName(quoteSnapshot)
		}
		if signerName == "" {
			return nil, fmt.Errorf("%w: signer_name is required", ErrInvalidInput)
		}

		snap, err := json.Marshal(contractSnapshot{
			Schema:          "contract.v1",
			TemplateVersion: templateVersion,
			GeneratedAt:     time.Now().UTC(),
			Quote:           quoteSnapshot,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal contract snapshot: %w", err)
		}
		sum := sha256.Sum256(snap)

		row := tx.QueryRow(ctx, `
			INSERT INTO contracts (tenant_id, project_id, quote_id, quote_version, status,
				template_version, snapshot, document_hash, signer_name, signer_contact,
				included_rounds, created_by)
			VALUES ($1,$2,$3,$4,'rendered',$5,$6,$7,$8,$9,$10,$11)
			RETURNING `+columns,
			tenantID, in.ProjectID, in.QuoteID, quoteVersion, templateVersion, snap,
			hex.EncodeToString(sum[:]), signerName, signerContact, includedRounds, actor)
		c, err := scan(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrActiveContract
			}
			return nil, fmt.Errorf("insert contract: %w", err)
		}

		// The frozen quote is the financial source of truth once a contract exists.
		// Project.version is untouched: projections are not user edits.
		if _, err := tx.Exec(ctx, `
			UPDATE projects SET currency = $3, total_amount_cents = $4, deposit_cents = $5,
				balance_cents = $6, contract_status = 'rendered', updated_at = now()
			WHERE id = $1 AND tenant_id = $2`,
			in.ProjectID, tenantID, quoteCurrency, quoteTotal, quoteDeposit, quoteTotal-quoteDeposit); err != nil {
			return nil, fmt.Errorf("sync project amounts: %w", err)
		}

		if err := audit.Write(ctx, tx, tenantID, &actor, "contract.created", "contract", c.ID.String(), ip,
			map[string]any{"project_id": c.ProjectID, "quote_id": c.QuoteID, "quote_version": c.QuoteVersion}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "contract", c.ID.String(), "contract.created",
			map[string]any{"project_id": c.ProjectID, "quote_id": c.QuoteID, "quote_version": c.QuoteVersion}); err != nil {
			return nil, err
		}
		return c, nil
	})
}

// Send creates the provider signing task and records it before the contract
// becomes "sent". The provider call runs outside the transaction so a slow
// vendor never holds row locks.
//
// The contract is reserved as "submitting" before the vendor call. If the
// process dies between the call and the local record, a later retry resumes
// from "submitting": providers must key the task by contract id, so the retry
// gets the existing task back instead of creating a second one. If the local
// write fails while the vendor task exists, the task is revoked and the
// contract returns to "rendered"; when revocation is impossible the operator is
// paged with the task id.
func (s *Service) Send(ctx context.Context, tenantID, actor, contractID uuid.UUID, ip string, expectedVersion int) (*Contract, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}

	pre, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*sendPlan, error) {
		c, err := lockContract(ctx, tx, tenantID, contractID)
		if err != nil {
			return nil, err
		}
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, c.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: project is cancelled or archived", ErrIllegalState)
			}
			return nil, fmt.Errorf("check contract project state: %w", err)
		}
		var title string
		if err := tx.QueryRow(ctx, `SELECT title FROM projects WHERE id = $1 AND tenant_id = $2`,
			c.ProjectID, tenantID).Scan(&title); err != nil {
			return nil, fmt.Errorf("load project for send: %w", err)
		}
		plan := &sendPlan{
			Version:       c.Version,
			Status:        c.Status,
			ProjectID:     c.ProjectID,
			Title:         title + " 拍摄合同",
			SignerName:    c.SignerName,
			SignerContact: c.SignerContact,
		}
		if c.Status == StatusSubmitting {
			if time.Since(c.UpdatedAt) < submittingStaleAfter {
				return nil, ErrSendInProgress
			}
			// Resume a send that died before the task was recorded. Refreshing
			// updated_at fences concurrent retries while the vendor call runs.
			if _, err := tx.Exec(ctx, `
				UPDATE contracts SET updated_at = now()
				WHERE id = $1 AND tenant_id = $2 AND status = 'submitting'`,
				contractID, tenantID); err != nil {
				return nil, fmt.Errorf("resume submitting contract: %w", err)
			}
			return plan, nil
		}
		if c.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanSend(c.Status) {
			return nil, fmt.Errorf("%w: cannot send a %s contract", ErrIllegalState, c.Status)
		}
		if plan.SignerName == "" {
			return nil, fmt.Errorf("%w: signer_name is required", ErrInvalidInput)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE contracts SET status = 'submitting', updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND status = $3`,
			contractID, tenantID, c.Status); err != nil {
			return nil, fmt.Errorf("reserve contract for sending: %w", err)
		}
		return plan, nil
	})
	if err != nil {
		return nil, err
	}

	task, err := s.provider.CreateSignTask(ctx, esign.SignTaskRequest{
		ContractID:    contractID.String(),
		ProjectID:     pre.ProjectID.String(),
		Title:         pre.Title,
		SignerName:    pre.SignerName,
		SignerContact: pre.SignerContact,
	})
	if err != nil {
		s.releaseSubmitting(ctx, tenantID, &actor, ip, contractID, "provider error: "+err.Error())
		return nil, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	if task == nil || task.TaskID == "" || len(task.TaskID) > 256 {
		s.releaseSubmitting(ctx, tenantID, &actor, ip, contractID, "provider returned an invalid task")
		return nil, fmt.Errorf("%w: provider returned an invalid task", ErrProvider)
	}

	sent, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Contract, error) {
		c, err := lockContract(ctx, tx, tenantID, contractID)
		if err != nil {
			return nil, err
		}
		if c.Status != StatusSubmitting || c.Version != pre.Version {
			return nil, fmt.Errorf("%w: contract changed while the signing task was being created", ErrIllegalState)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO esign_tasks (provider, sign_task_id, tenant_id, contract_id)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (provider, sign_task_id) DO NOTHING`,
			s.provider.Name(), task.TaskID, tenantID, contractID); err != nil {
			return nil, fmt.Errorf("record esign task: %w", err)
		}
		row := tx.QueryRow(ctx, `
			UPDATE contracts SET status = 'sent', sign_task_id = $3, sign_url = $4,
				sent_at = now(), version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND status = 'submitting'
			RETURNING `+columns, contractID, tenantID, task.TaskID, task.SignURL)
		sent, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("send contract: %w", err)
		}
		if err := setProjectStatus(ctx, tx, tenantID, sent.ProjectID, StatusSent); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "contract.sent", "contract", contractID.String(), ip,
			map[string]any{"project_id": sent.ProjectID, "sign_task_id": task.TaskID}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "contract", contractID.String(), "contract.sent",
			map[string]any{"project_id": sent.ProjectID, "sign_task_id": task.TaskID}); err != nil {
			return nil, err
		}
		return sent, nil
	})
	if err != nil {
		s.compensateSend(ctx, tenantID, contractID, task, err)
		return nil, err
	}
	return sent, nil
}

// releaseSubmitting returns a reserved contract to "rendered" after the vendor
// call produced no usable task. It is a no-op when the row already moved on.
func (s *Service) releaseSubmitting(ctx context.Context, tenantID uuid.UUID, actor *uuid.UUID, ip string, contractID uuid.UUID, reason string) {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE contracts SET status = 'rendered', updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND status = 'submitting'`, contractID, tenantID)
		if err != nil {
			return fmt.Errorf("revert submitting contract: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return audit.Write(ctx, tx, tenantID, actor, "contract.send_reverted", "contract",
			contractID.String(), ip, map[string]any{"reason": reason})
	})
	if err != nil {
		s.log.Error("revert contract after failed send", "contract_id", contractID, "err", err)
	}
}

// compensateSend runs after the signing task exists at the provider but could
// not be recorded locally. It only revokes the task when the local row proves
// the send did not land; an ambiguous commit (the row is already sent with this
// task) is left alone.
func (s *Service) compensateSend(ctx context.Context, tenantID, contractID uuid.UUID, task *esign.SignTask, cause error) {
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	var status, recordedID string
	if err := db.WithTenant(opCtx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(opCtx, `
			SELECT status, sign_task_id FROM contracts WHERE id = $1 AND tenant_id = $2`,
			contractID, tenantID).Scan(&status, &recordedID)
	}); err != nil {
		// Without a trustworthy local state, revoking could break a contract
		// that was in fact recorded. Leave it "submitting"; the stale retry
		// re-creates the same task and completes the send.
		s.log.Error("cannot verify contract state before compensating signing task",
			"contract_id", contractID, "sign_task_id", task.TaskID, "err", err)
		return
	}
	if status == StatusSent && recordedID == task.TaskID {
		return
	}

	canceller, ok := s.provider.(esign.TaskCanceller)
	if ok {
		if cerr := canceller.CancelSignTask(opCtx, task.TaskID); cerr == nil {
			s.releaseSubmitting(opCtx, tenantID, nil, "", contractID,
				"local record failed, signing task revoked: "+cause.Error())
			return
		} else {
			cause = fmt.Errorf("%w; revoking the signing task failed: %v", cause, cerr)
		}
	}
	s.raiseOrphanAlert(opCtx, tenantID, contractID, task, cause)
}

// raiseOrphanAlert pages operators when a provider task cannot be matched to a
// local contract. The task id is the only handle the vendor console has.
func (s *Service) raiseOrphanAlert(ctx context.Context, tenantID, contractID uuid.UUID, task *esign.SignTask, cause error) {
	s.log.Error("orphaned e-sign task", "contract_id", contractID,
		"sign_task_id", task.TaskID, "err", cause)
	s.raiseContractAlert(ctx, tenantID, contractID, "esign_task_orphaned",
		"esign_orphan:"+contractID.String(),
		"Signing task exists at the provider but the contract was not marked as sent",
		map[string]any{"sign_task_id": task.TaskID, "error": cause.Error()})
}

// raiseContractAlert records a critical operator alert tied to a contract. The
// project id is resolved here so call sites do not have to carry it.
func (s *Service) raiseContractAlert(ctx context.Context, tenantID, contractID uuid.UUID, code, dedupeKey, title string, detail map[string]any) {
	if s.alerts == nil {
		return
	}
	_, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*alert.Alert, error) {
		var projectID uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT project_id FROM contracts WHERE id = $1 AND tenant_id = $2`,
			contractID, tenantID).Scan(&projectID); err != nil {
			return nil, fmt.Errorf("load contract for alert: %w", err)
		}
		detail["project_id"] = projectID.String()
		detail["contract_id"] = contractID.String()
		return s.alerts.Raise(ctx, tx, tenantID, alert.Input{
			ProjectID:   &projectID,
			SubjectType: "contract",
			SubjectID:   contractID.String(),
			Code:        code,
			Severity:    alert.SeverityCritical,
			DedupeKey:   dedupeKey,
			Title:       title,
			Detail:      detail,
		})
	})
	if err != nil {
		s.log.Error("raise contract alert", "contract_id", contractID, "code", code, "err", err)
	}
}

type sendPlan struct {
	Version       int
	Status        string
	ProjectID     uuid.UUID
	Title         string
	SignerName    string
	SignerContact string
}

func (s *Service) Void(ctx context.Context, tenantID, actor, contractID uuid.UUID, ip string, expectedVersion int, reason string) (*Contract, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required to void a contract", ErrInvalidInput)
	}
	if len(reason) > 500 {
		return nil, fmt.Errorf("%w: reason is too long", ErrInvalidInput)
	}

	updated, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Contract, error) {
		c, err := lockContract(ctx, tx, tenantID, contractID)
		if err != nil {
			return nil, err
		}
		if c.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanVoid(c.Status) {
			return nil, fmt.Errorf("%w: cannot void a %s contract", ErrIllegalState, c.Status)
		}
		row := tx.QueryRow(ctx, `
			UPDATE contracts SET status = 'void', void_reason = $3,
				version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, contractID, tenantID, reason)
		updated, err := scan(row)
		if err != nil {
			return nil, fmt.Errorf("void contract: %w", err)
		}
		if err := setProjectStatus(ctx, tx, tenantID, updated.ProjectID, "none"); err != nil {
			return nil, err
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "contract.voided", "contract", contractID.String(), ip,
			map[string]any{"project_id": updated.ProjectID, "reason": reason}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "contract", contractID.String(), "contract.voided",
			map[string]any{"project_id": updated.ProjectID, "reason": reason}); err != nil {
			return nil, err
		}
		return updated, nil
	})
	if err != nil {
		return nil, err
	}
	// The local void is committed first: revoking the vendor task before the
	// commit could leave a sent contract whose sign link no longer works.
	s.revokeSignTask(ctx, tenantID, contractID, updated.SignTaskID)
	return updated, nil
}

// revokeSignTask cancels the provider-side task behind a voided contract. It is
// best effort by design: the void is already committed, so a failure is paged
// to operators (the vendor console must cancel the task manually) instead of
// failing an operation the customer already sees as void.
func (s *Service) revokeSignTask(ctx context.Context, tenantID, contractID uuid.UUID, taskID string) {
	if taskID == "" {
		return
	}
	canceller, ok := s.provider.(esign.TaskCanceller)
	if !ok {
		return
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := canceller.CancelSignTask(opCtx, taskID); err != nil {
		s.log.Error("revoke signing task of voided contract", "contract_id", contractID,
			"sign_task_id", taskID, "err", err)
		s.raiseContractAlert(opCtx, tenantID, contractID, "esign_task_revoke_failed",
			"esign_revoke:"+contractID.String(),
			"Voided contract still has a live signing task at the provider",
			map[string]any{"sign_task_id": taskID, "error": err.Error()})
	}
}

// HandleTaskEvent applies a verified provider event exactly once. The signing
// dispatcher has already verified the provider signature and resolved the
// tenant; this method only trusts normalised, validated events.
func (s *Service) HandleTaskEvent(ctx context.Context, tenantID, contractID uuid.UUID, providerName string, ev *esign.CallbackEvent) (*esign.TaskEventResult, error) {
	if providerName != s.provider.Name() {
		return nil, ErrUnknownProvider
	}
	payload := ev.Raw
	if !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	result := &esign.TaskEventResult{SubjectType: "contract", SubjectID: contractID}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*esign.TaskEventResult, error) {
		c, err := lockContract(ctx, tx, tenantID, contractID)
		if err != nil {
			return nil, err
		}
		ct, err := tx.Exec(ctx, `
			INSERT INTO esign_events (provider, event_id, sign_task_id, event_type, payload)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (provider, event_id) DO NOTHING`,
			providerName, ev.EventID, ev.TaskID, ev.Type, payload)
		if err != nil {
			return nil, fmt.Errorf("record esign event: %w", err)
		}
		if ct.RowsAffected() == 0 {
			result.Status = "duplicate"
			return result, nil
		}
		if c.SignTaskID != ev.TaskID {
			// A stale task from a superseded contract must never move state.
			result.Status = "ignored"
			return result, nil
		}

		switch ev.Type {
		case esign.EventSigning:
			if c.Status != StatusSent {
				result.Status = "ignored"
				return result, nil
			}
			if _, err := tx.Exec(ctx, `
				UPDATE contracts SET status = 'signing', version = version + 1, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`, contractID, tenantID); err != nil {
				return nil, fmt.Errorf("mark contract signing: %w", err)
			}
			if err := setProjectStatus(ctx, tx, tenantID, c.ProjectID, StatusSigning); err != nil {
				return nil, err
			}

		case esign.EventSigned:
			if !CanSign(c.Status) {
				result.Status = "ignored"
				return result, nil
			}
			signedAt := ev.SignedAt
			if signedAt == nil {
				now := time.Now().UTC()
				signedAt = &now
			}
			if _, err := tx.Exec(ctx, `
				UPDATE contracts SET status = 'signed',
					signer_name = CASE WHEN $3 = '' THEN signer_name ELSE $3 END,
					signed_at = $4,
					signed_document_hash = CASE WHEN $5 = '' THEN signed_document_hash ELSE $5 END,
					evidence = $6, version = version + 1, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`,
				contractID, tenantID, ev.SignerName, signedAt, ev.DocumentSHA256, payload); err != nil {
				return nil, fmt.Errorf("mark contract signed: %w", err)
			}
			if err := setProjectStatus(ctx, tx, tenantID, c.ProjectID, StatusSigned); err != nil {
				return nil, err
			}

		case esign.EventVoid:
			if !CanVoid(c.Status) {
				result.Status = "ignored"
				return result, nil
			}
			reason := strings.TrimSpace(ev.Reason)
			if reason == "" {
				reason = "voided by e-sign provider"
			}
			if _, err := tx.Exec(ctx, `
				UPDATE contracts SET status = 'void', void_reason = $3, evidence = $4,
					version = version + 1, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`,
				contractID, tenantID, reason, payload); err != nil {
				return nil, fmt.Errorf("void contract by callback: %w", err)
			}
			if err := setProjectStatus(ctx, tx, tenantID, c.ProjectID, "none"); err != nil {
				return nil, err
			}
		}

		detail := map[string]any{"project_id": c.ProjectID, "event_id": ev.EventID, "event_type": ev.Type}
		if ev.SignerName != "" {
			detail["signer_name"] = ev.SignerName
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "contract."+ev.Type, "contract", contractID.String(), "", detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "contract", contractID.String(), "contract."+ev.Type, detail); err != nil {
			return nil, err
		}
		result.Status = "processed"
		return result, nil
	})
}

func (s *Service) Get(ctx context.Context, tenantID, contractID uuid.UUID) (*Contract, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Contract, error) {
		row := tx.QueryRow(ctx, `SELECT `+columns+` FROM contracts WHERE id = $1 AND tenant_id = $2`,
			contractID, tenantID)
		c, err := scan(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("get contract: %w", err)
		}
		return c, nil
	})
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status string, limit, offset int) ([]Contract, int, error) {
	if status != "" && !ValidStatus(status) {
		return nil, 0, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
	}
	var (
		items []Contract
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const filter = `tenant_id = $1 AND ($2::uuid IS NULL OR project_id = $2)
			AND ($3::text = '' OR status = $3::text)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM contracts WHERE `+filter,
			tenantID, projectID, status).Scan(&total); err != nil {
			return fmt.Errorf("count contracts: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM contracts WHERE `+filter+`
			ORDER BY created_at DESC LIMIT $4 OFFSET $5`, tenantID, projectID, status, limit, offset)
		if err != nil {
			return fmt.Errorf("list contracts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scan(rows)
			if err != nil {
				return fmt.Errorf("scan contract: %w", err)
			}
			items = append(items, *c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []Contract{}
	}
	return items, total, nil
}

func lockContract(ctx context.Context, tx pgx.Tx, tenantID, contractID uuid.UUID) (*Contract, error) {
	row := tx.QueryRow(ctx, `SELECT `+columns+` FROM contracts
		WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, contractID, tenantID)
	c, err := scan(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lock contract: %w", err)
	}
	return c, nil
}

// setProjectStatus updates the projection without touching project.version:
// callbacks and contract lifecycle events are not user edits.
func setProjectStatus(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID, status string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE projects SET contract_status = $3, updated_at = now()
		WHERE id = $1 AND tenant_id = $2`, projectID, tenantID, status); err != nil {
		return fmt.Errorf("sync project contract status: %w", err)
	}
	return nil
}

type contractSnapshot struct {
	Schema          string          `json:"schema"`
	TemplateVersion string          `json:"template_version"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Quote           json.RawMessage `json:"quote"`
}

func snapshotClientName(raw json.RawMessage) string {
	var doc struct {
		Client *struct {
			Name string `json:"name"`
		} `json:"client"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Client == nil {
		return ""
	}
	return strings.TrimSpace(doc.Client.Name)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scan(row rowScanner) (*Contract, error) {
	var c Contract
	if err := row.Scan(&c.ID, &c.ProjectID, &c.QuoteID, &c.QuoteVersion, &c.Status,
		&c.TemplateVersion, &c.Snapshot, &c.DocumentKey, &c.DocumentHash,
		&c.SignTaskID, &c.SignURL, &c.SignedDocumentKey, &c.SignedDocumentHash,
		&c.SignerName, &c.SignerContact, &c.Evidence, &c.SentAt, &c.SignedAt,
		&c.VoidReason, &c.IncludedRounds, &c.Version, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if len(c.Snapshot) == 0 {
		c.Snapshot = json.RawMessage(`{}`)
	}
	if len(c.Evidence) == 0 {
		c.Evidence = json.RawMessage(`{}`)
	}
	return &c, nil
}
