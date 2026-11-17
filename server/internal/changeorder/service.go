package changeorder

import (
	"context"
	"crypto/rand"
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
	"photography-workflow/server/internal/payment"
	projectstate "photography-workflow/server/internal/project"
)

var (
	ErrNotFound          = errors.New("change order not found")
	ErrProjectNotFound   = errors.New("project not found")
	ErrInvalidInput      = errors.New("invalid change order input")
	ErrVersionConflict   = errors.New("change order version conflict")
	ErrIllegalState      = errors.New("illegal change order state")
	ErrActiveChangeOrder = errors.New("project already has an active change order")
	ErrPaid              = errors.New("change order has already been paid")
	ErrOrderInProgress   = errors.New("change order payment order is being created")
	ErrPayment           = errors.New("change order payment error")
	ErrUnknownProvider   = errors.New("unknown e-sign provider")
)

const columns = `id, project_id, order_no, status, currency, amount_cents, reason, snapshot,
	sign_task_id, sign_url, signer_name, signer_contact, signed_document_hash, signed_at,
	payment_order_id, applied_at, cancelled_at, void_reason, version, created_by, created_at, updated_at`

const itemColumns = `id, change_order_id, kind, description, quantity, unit_price_cents, amount_cents, sort`

type ChangeOrder struct {
	ID                 uuid.UUID       `json:"id"`
	ProjectID          uuid.UUID       `json:"project_id"`
	OrderNo            string          `json:"order_no"`
	Status             string          `json:"status"`
	Currency           string          `json:"currency"`
	AmountCents        int64           `json:"amount_cents"`
	Reason             string          `json:"reason"`
	Snapshot           json.RawMessage `json:"snapshot,omitempty"`
	SignTaskID         string          `json:"sign_task_id,omitempty"`
	SignURL            string          `json:"sign_url,omitempty"`
	SignerName         string          `json:"signer_name"`
	SignerContact      string          `json:"signer_contact"`
	SignedDocumentHash string          `json:"signed_document_hash,omitempty"`
	SignedAt           *time.Time      `json:"signed_at,omitempty"`
	PaymentOrderID     *uuid.UUID      `json:"payment_order_id,omitempty"`
	AppliedAt          *time.Time      `json:"applied_at,omitempty"`
	CancelledAt        *time.Time      `json:"cancelled_at,omitempty"`
	VoidReason         string          `json:"void_reason,omitempty"`
	Version            int             `json:"version"`
	CreatedBy          *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	Items              []Item          `json:"items,omitempty"`
}

type Item struct {
	ID             uuid.UUID `json:"id"`
	ChangeOrderID  uuid.UUID `json:"change_order_id"`
	Kind           string    `json:"kind"`
	Description    string    `json:"description"`
	Quantity       int       `json:"quantity"`
	UnitPriceCents int64     `json:"unit_price_cents"`
	AmountCents    int64     `json:"amount_cents"`
	Sort           int       `json:"sort"`
}

type ItemInput struct {
	Kind           string
	Description    string
	Quantity       int
	UnitPriceCents int64
}

type CreateInput struct {
	ProjectID     uuid.UUID
	Reason        string
	SignerName    string
	SignerContact string
	Items         []ItemInput
}

type Service struct {
	pool     *pgxpool.Pool
	provider esign.Provider
	payments *payment.Service
	alerts   *alert.Service
	log      *slog.Logger
}

func NewService(pool *pgxpool.Pool, provider esign.Provider, payments *payment.Service, alerts *alert.Service, log *slog.Logger) *Service {
	return &Service{pool: pool, provider: provider, payments: payments, alerts: alerts, log: log}
}

type preparedItem struct {
	Kind           string
	Description    string
	Quantity       int
	UnitPriceCents int64
	AmountCents    int64
}

func prepareItems(items []ItemInput) ([]preparedItem, int64, error) {
	if len(items) == 0 {
		return nil, 0, fmt.Errorf("%w: at least one item is required", ErrInvalidInput)
	}
	if len(items) > 50 {
		return nil, 0, fmt.Errorf("%w: at most 50 items are allowed", ErrInvalidInput)
	}
	out := make([]preparedItem, 0, len(items))
	var total int64
	for i, it := range items {
		kind := strings.TrimSpace(it.Kind)
		if kind == "" {
			kind = ItemOther
		}
		if !ValidItemKind(kind) {
			return nil, 0, fmt.Errorf("%w: item %d has an unknown kind %q", ErrInvalidInput, i+1, it.Kind)
		}
		desc := strings.TrimSpace(it.Description)
		if desc == "" || len(desc) > 300 {
			return nil, 0, fmt.Errorf("%w: item %d needs a description of at most 300 characters", ErrInvalidInput, i+1)
		}
		if it.Quantity <= 0 || it.Quantity > 1000 {
			return nil, 0, fmt.Errorf("%w: item %d quantity must be between 1 and 1000", ErrInvalidInput, i+1)
		}
		if it.UnitPriceCents < 0 || it.UnitPriceCents > 1_000_000_000 {
			return nil, 0, fmt.Errorf("%w: item %d unit price is out of range", ErrInvalidInput, i+1)
		}
		amount := int64(it.Quantity) * it.UnitPriceCents
		if amount <= 0 {
			return nil, 0, fmt.Errorf("%w: item %d has a zero amount", ErrInvalidInput, i+1)
		}
		total += amount
		out = append(out, preparedItem{
			Kind: kind, Description: desc, Quantity: it.Quantity,
			UnitPriceCents: it.UnitPriceCents, AmountCents: amount,
		})
	}
	if total <= 0 {
		return nil, 0, fmt.Errorf("%w: the change order amount must be positive", ErrInvalidInput)
	}
	return out, total, nil
}

type orderSnapshot struct {
	Schema      string         `json:"schema"`
	OrderNo     string         `json:"order_no"`
	ProjectID   uuid.UUID      `json:"project_id"`
	Currency    string         `json:"currency"`
	AmountCents int64          `json:"amount_cents"`
	Reason      string         `json:"reason"`
	GeneratedAt time.Time      `json:"generated_at"`
	Items       []snapshotItem `json:"items"`
}

type snapshotItem struct {
	Kind           string `json:"kind"`
	Description    string `json:"description"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
	AmountCents    int64  `json:"amount_cents"`
}

// Create records a draft amendment on a signed contract. The total is derived
// from the line items so the header can never disagree with its breakdown. A
// completed project may still be amended (extra retouch/re-shoot), which is the
// main commercial case for change orders.
func (s *Service) Create(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateInput) (*ChangeOrder, error) {
	if in.ProjectID == uuid.Nil {
		return nil, fmt.Errorf("%w: project_id is required", ErrInvalidInput)
	}
	items, total, err := prepareItems(in.Items)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	if len(reason) > 500 {
		return nil, fmt.Errorf("%w: reason is too long", ErrInvalidInput)
	}
	signerName := strings.TrimSpace(in.SignerName)
	signerContact := strings.TrimSpace(in.SignerContact)
	if len(signerName) > 120 || len(signerContact) > 200 {
		return nil, fmt.Errorf("%w: signer fields are too long", ErrInvalidInput)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ChangeOrder, error) {
		var (
			currency       string
			contractStatus string
			state          string
		)
		if err := tx.QueryRow(ctx, `
			SELECT currency, contract_status, state FROM projects
			WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			in.ProjectID, tenantID).Scan(&currency, &contractStatus, &state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrProjectNotFound
			}
			return nil, fmt.Errorf("lock project: %w", err)
		}
		if err := projectstate.EnsureOperational(ctx, tx, tenantID, in.ProjectID); err != nil {
			if errors.Is(err, projectstate.ErrClosed) {
				return nil, fmt.Errorf("%w: project is cancelled or archived", ErrIllegalState)
			}
			return nil, fmt.Errorf("check project state: %w", err)
		}
		if contractStatus != "signed" {
			return nil, fmt.Errorf("%w: a signed contract is required before amending it", ErrIllegalState)
		}
		if state == "archived" || state == "cancelled" {
			return nil, fmt.Errorf("%w: project is %s", ErrIllegalState, state)
		}

		orderNo, err := newOrderNo()
		if err != nil {
			return nil, err
		}
		snapItems := make([]snapshotItem, len(items))
		for i, it := range items {
			snapItems[i] = snapshotItem{
				Kind: it.Kind, Description: it.Description, Quantity: it.Quantity,
				UnitPriceCents: it.UnitPriceCents, AmountCents: it.AmountCents,
			}
		}
		snap, err := json.Marshal(orderSnapshot{
			Schema: "change_order.v1", OrderNo: orderNo, ProjectID: in.ProjectID,
			Currency: currency, AmountCents: total, Reason: reason,
			GeneratedAt: time.Now().UTC(), Items: snapItems,
		})
		if err != nil {
			return nil, fmt.Errorf("marshal change order snapshot: %w", err)
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO change_orders (tenant_id, project_id, order_no, status, currency,
				amount_cents, reason, snapshot, signer_name, signer_contact, created_by)
			VALUES ($1,$2,$3,'draft',$4,$5,$6,$7,$8,$9,$10)
			RETURNING `+columns,
			tenantID, in.ProjectID, orderNo, currency, total, reason, snap,
			signerName, signerContact, actor)
		co, err := scanChangeOrder(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, ErrActiveChangeOrder
			}
			return nil, fmt.Errorf("insert change order: %w", err)
		}
		for i, it := range items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO change_order_items (tenant_id, change_order_id, kind, description,
					quantity, unit_price_cents, amount_cents, sort)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				tenantID, co.ID, it.Kind, it.Description, it.Quantity,
				it.UnitPriceCents, it.AmountCents, i); err != nil {
				return nil, fmt.Errorf("insert change order item: %w", err)
			}
		}
		if co.Items, err = loadItems(ctx, tx, tenantID, co.ID); err != nil {
			return nil, err
		}
		detail := map[string]any{
			"project_id": co.ProjectID, "order_no": co.OrderNo,
			"amount_cents": co.AmountCents, "currency": co.Currency,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "change_order.created", "change_order", co.ID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "change_order", co.ID.String(), "change_order.created", detail); err != nil {
			return nil, err
		}
		return co, nil
	})
}

type sendPlan struct {
	Version       int
	Status        string
	ProjectID     uuid.UUID
	OrderNo       string
	Title         string
	SignerName    string
	SignerContact string
}

// Send creates the provider signing task outside the transaction, then records
// the task and moves the amendment to awaiting_signature.
func (s *Service) Send(ctx context.Context, tenantID, actor, changeOrderID uuid.UUID, ip string, expectedVersion int) (*ChangeOrder, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}

	var plan sendPlan
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var title string
		if err := tx.QueryRow(ctx, `
			SELECT c.version, c.status, c.project_id, c.order_no, c.signer_name,
				c.signer_contact, p.title
			FROM change_orders c JOIN projects p ON p.id = c.project_id AND p.tenant_id = c.tenant_id
			WHERE c.id = $1 AND c.tenant_id = $2`,
			changeOrderID, tenantID).Scan(&plan.Version, &plan.Status, &plan.ProjectID,
			&plan.OrderNo, &plan.SignerName, &plan.SignerContact, &title); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("load change order for send: %w", err)
		}
		plan.Title = title + " 变更单 " + plan.OrderNo
		if plan.Version != expectedVersion {
			return ErrVersionConflict
		}
		if !CanSend(plan.Status) {
			return fmt.Errorf("%w: cannot send a %s change order", ErrIllegalState, plan.Status)
		}
		if plan.SignerName == "" {
			return fmt.Errorf("%w: signer_name is required", ErrInvalidInput)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	task, err := s.provider.CreateSignTask(ctx, esign.SignTaskRequest{
		ProjectID:     plan.ProjectID.String(),
		Title:         plan.Title,
		SignerName:    plan.SignerName,
		SignerContact: plan.SignerContact,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPayment, err)
	}
	if task == nil || task.TaskID == "" || len(task.TaskID) > 256 {
		return nil, fmt.Errorf("%w: provider returned an invalid task", ErrPayment)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ChangeOrder, error) {
		co, err := lockChangeOrder(ctx, tx, tenantID, changeOrderID)
		if err != nil {
			return nil, err
		}
		if co.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if !CanSend(co.Status) {
			return nil, fmt.Errorf("%w: cannot send a %s change order", ErrIllegalState, co.Status)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO esign_tasks (provider, sign_task_id, tenant_id, change_order_id)
			VALUES ($1,$2,$3,$4)`,
			s.provider.Name(), task.TaskID, tenantID, changeOrderID); err != nil {
			return nil, fmt.Errorf("record esign task: %w", err)
		}
		row := tx.QueryRow(ctx, `
			UPDATE change_orders SET status = 'awaiting_signature', sign_task_id = $3,
				sign_url = $4, version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, changeOrderID, tenantID, task.TaskID, task.SignURL)
		sent, err := scanChangeOrder(row)
		if err != nil {
			return nil, fmt.Errorf("send change order: %w", err)
		}
		detail := map[string]any{
			"project_id": sent.ProjectID, "order_no": sent.OrderNo, "sign_task_id": task.TaskID,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "change_order.sent", "change_order", changeOrderID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "change_order", changeOrderID.String(), "change_order.sent", detail); err != nil {
			return nil, err
		}
		return sent, nil
	})
}

// HandleTaskEvent applies a verified signing callback. A signed amendment
// automatically tries to open its supplement payment order after commit; a
// failure there never undoes the signature, it surfaces as an alert and can be
// retried through EnsurePaymentOrder.
func (s *Service) HandleTaskEvent(ctx context.Context, tenantID, changeOrderID uuid.UUID, providerName string, ev *esign.CallbackEvent) (*esign.TaskEventResult, error) {
	if providerName != s.provider.Name() {
		return nil, ErrUnknownProvider
	}
	payload := ev.Raw
	if !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	result := &esign.TaskEventResult{SubjectType: "change_order", SubjectID: changeOrderID}
	var signedNow bool

	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*esign.TaskEventResult, error) {
		co, err := lockChangeOrder(ctx, tx, tenantID, changeOrderID)
		if err != nil {
			return nil, err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO esign_events (provider, event_id, sign_task_id, event_type, payload)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (provider, event_id) DO NOTHING`,
			providerName, ev.EventID, ev.TaskID, ev.Type, payload)
		if err != nil {
			return nil, fmt.Errorf("record esign event: %w", err)
		}
		if tag.RowsAffected() == 0 {
			result.Status = "duplicate"
			return result, nil
		}
		if co.SignTaskID != ev.TaskID {
			result.Status = "ignored"
			return result, nil
		}

		switch ev.Type {
		case esign.EventSigning:
			if co.Status != StatusAwaitingSignature {
				result.Status = "ignored"
				return result, nil
			}

		case esign.EventSigned:
			if co.Status != StatusAwaitingSignature {
				result.Status = "ignored"
				return result, nil
			}
			signedAt := ev.SignedAt
			if signedAt == nil {
				now := time.Now().UTC()
				signedAt = &now
			}
			if _, err := tx.Exec(ctx, `
				UPDATE change_orders SET status = 'signed',
					signer_name = CASE WHEN $3 = '' THEN signer_name ELSE $3 END,
					signed_at = $4,
					signed_document_hash = CASE WHEN $5 = '' THEN signed_document_hash ELSE $5 END,
					version = version + 1, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`,
				changeOrderID, tenantID, ev.SignerName, signedAt, ev.DocumentSHA256); err != nil {
				return nil, fmt.Errorf("mark change order signed: %w", err)
			}
			signedNow = true

		case esign.EventVoid:
			if co.Status != StatusAwaitingSignature {
				result.Status = "ignored"
				return result, nil
			}
			reason := strings.TrimSpace(ev.Reason)
			if reason == "" {
				reason = "voided by e-sign provider"
			}
			if _, err := tx.Exec(ctx, `
				UPDATE change_orders SET status = 'cancelled', void_reason = $3,
					cancelled_at = now(), version = version + 1, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`, changeOrderID, tenantID, reason); err != nil {
				return nil, fmt.Errorf("cancel change order by callback: %w", err)
			}
		}

		detail := map[string]any{
			"project_id": co.ProjectID, "event_id": ev.EventID, "event_type": ev.Type,
		}
		if ev.SignerName != "" {
			detail["signer_name"] = ev.SignerName
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "change_order."+ev.Type, "change_order", changeOrderID.String(), "", detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "change_order", changeOrderID.String(), "change_order."+ev.Type, detail); err != nil {
			return nil, err
		}
		result.Status = "processed"
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	if signedNow {
		if _, err := s.EnsurePaymentOrder(ctx, tenantID, uuid.Nil, "", changeOrderID); err != nil {
			s.log.Error("create supplement payment order after signing", "change_order_id", changeOrderID, "err", err)
		}
	}
	return res, nil
}

// EnsurePaymentOrder opens (or reuses) the supplement payment order for a
// signed amendment and records it on the change order. It is idempotent so the
// signed callback and the manual retry endpoint can both call it.
func (s *Service) EnsurePaymentOrder(ctx context.Context, tenantID, actor uuid.UUID, ip string, changeOrderID uuid.UUID) (*ChangeOrder, error) {
	current, err := s.get(ctx, tenantID, changeOrderID)
	if err != nil {
		return nil, err
	}
	switch current.Status {
	case StatusAwaitingPayment:
		if current.PaymentOrderID != nil {
			return current, nil
		}
	case StatusSigned:
	default:
		return nil, fmt.Errorf("%w: cannot collect a %s change order", ErrIllegalState, current.Status)
	}

	order, _, err := s.payments.CreateOrder(ctx, tenantID, actor, ip, payment.CreateOrderInput{
		ProjectID: current.ProjectID, Kind: payment.KindChange, ChangeOrderID: &changeOrderID,
	})
	if err != nil {
		s.raisePaymentFailure(ctx, tenantID, current, err)
		return nil, fmt.Errorf("%w: %v", ErrPayment, err)
	}

	var closeCreated bool
	updated, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ChangeOrder, error) {
		co, err := lockChangeOrder(ctx, tx, tenantID, changeOrderID)
		if err != nil {
			return nil, err
		}
		switch {
		case co.Status == StatusAwaitingPayment && co.PaymentOrderID != nil && *co.PaymentOrderID == order.ID:
			return co, nil
		case co.Status == StatusAwaitingPayment:
			// Another order exists for this amendment; never leave two live links.
			closeCreated = true
			return nil, ErrOrderInProgress
		case co.Status == StatusSigned && CanCollect(co.Status):
			row := tx.QueryRow(ctx, `
				UPDATE change_orders SET status = 'awaiting_payment', payment_order_id = $3,
					version = version + 1, updated_at = now()
				WHERE id = $1 AND tenant_id = $2
				RETURNING `+columns, changeOrderID, tenantID, order.ID)
			updated, err := scanChangeOrder(row)
			if err != nil {
				return nil, fmt.Errorf("record supplement payment order: %w", err)
			}
			detail := map[string]any{
				"project_id": updated.ProjectID, "order_no": updated.OrderNo,
				"payment_order_id": order.ID, "amount_cents": updated.AmountCents,
			}
			if err := audit.Write(ctx, tx, tenantID, callerActor(actor), "change_order.payment_created", "change_order", changeOrderID.String(), ip, detail); err != nil {
				return nil, err
			}
			if err := outbox.Emit(ctx, tx, tenantID, "change_order", changeOrderID.String(), "change_order.payment_created", detail); err != nil {
				return nil, err
			}
			return updated, nil
		case co.Status == StatusApplied:
			// The payment settled in the window before this CAS; keep the link.
			row := tx.QueryRow(ctx, `
				UPDATE change_orders SET payment_order_id = $3, updated_at = now()
				WHERE id = $1 AND tenant_id = $2
				RETURNING `+columns, changeOrderID, tenantID, order.ID)
			linked, err := scanChangeOrder(row)
			if err != nil {
				return nil, fmt.Errorf("link settled supplement order: %w", err)
			}
			return linked, nil
		default:
			closeCreated = true
			return nil, fmt.Errorf("%w: cannot collect a %s change order", ErrIllegalState, co.Status)
		}
	})
	if closeCreated {
		if _, cerr := s.payments.CloseOrder(ctx, tenantID, actor, order.ID, ip); cerr != nil {
			s.log.Error("close orphaned supplement order", "order_id", order.ID, "err", cerr)
		}
	}
	return updated, err
}

// Cancel closes the live supplement order at the channel first, then cancels
// the amendment. A captured payment wins the race: the change order is paid and
// cancellation is refused.
func (s *Service) Cancel(ctx context.Context, tenantID, actor, changeOrderID uuid.UUID, ip string, expectedVersion int, reason string) (*ChangeOrder, error) {
	if expectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version is required", ErrInvalidInput)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 500 {
		return nil, fmt.Errorf("%w: a cancellation reason is required and must be at most 500 characters", ErrInvalidInput)
	}

	current, err := s.get(ctx, tenantID, changeOrderID)
	if err != nil {
		return nil, err
	}
	if current.Version != expectedVersion {
		return nil, ErrVersionConflict
	}
	if current.Status == StatusApplied || current.Status == StatusReversed {
		return nil, ErrPaid
	}
	if !CanCancel(current.Status) {
		return nil, fmt.Errorf("%w: cannot cancel a %s change order", ErrIllegalState, current.Status)
	}

	liveOrderID, err := s.livePaymentOrder(ctx, tenantID, changeOrderID)
	if err != nil {
		return nil, err
	}
	if liveOrderID != nil {
		if _, err := s.payments.CloseOrder(ctx, tenantID, actor, *liveOrderID, ip); err != nil {
			switch {
			case errors.Is(err, payment.ErrOrderNotPending), errors.Is(err, payment.ErrOrderPaidAtProvider):
				return nil, ErrPaid
			default:
				return nil, fmt.Errorf("%w: %v", ErrPayment, err)
			}
		}
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ChangeOrder, error) {
		co, err := lockChangeOrder(ctx, tx, tenantID, changeOrderID)
		if err != nil {
			return nil, err
		}
		if co.Version != expectedVersion {
			return nil, ErrVersionConflict
		}
		if co.Status == StatusApplied || co.Status == StatusReversed {
			return nil, ErrPaid
		}
		if !CanCancel(co.Status) {
			return nil, fmt.Errorf("%w: cannot cancel a %s change order", ErrIllegalState, co.Status)
		}
		var live bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM payment_orders
				WHERE tenant_id = $1 AND change_order_id = $2 AND status IN ('creating','pending'))`,
			tenantID, changeOrderID).Scan(&live); err != nil {
			return nil, fmt.Errorf("check live supplement order: %w", err)
		}
		if live {
			// A payment order appeared while the channel order was being closed;
			// cancelling now would orphan it. The caller retries and closes it.
			return nil, ErrOrderInProgress
		}
		row := tx.QueryRow(ctx, `
			UPDATE change_orders SET status = 'cancelled', void_reason = $3,
				cancelled_at = now(), version = version + 1, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+columns, changeOrderID, tenantID, reason)
		cancelled, err := scanChangeOrder(row)
		if err != nil {
			return nil, fmt.Errorf("cancel change order: %w", err)
		}
		detail := map[string]any{
			"project_id": cancelled.ProjectID, "order_no": cancelled.OrderNo, "reason": reason,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "change_order.cancelled", "change_order", changeOrderID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "change_order", changeOrderID.String(), "change_order.cancelled", detail); err != nil {
			return nil, err
		}
		return cancelled, nil
	})
}

// OrderSettled implements payment.OrderHook: a settled supplement payment
// applies the amendment and projects its amount onto the project. It runs in
// the payment transaction, so a failure rolls the whole credit back.
func (s *Service) OrderSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, order *payment.Order) error {
	if order == nil || order.Kind != payment.KindChange || order.ChangeOrderID == nil {
		return nil
	}
	co, err := lockChangeOrder(ctx, tx, tenantID, *order.ChangeOrderID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case co.Status == StatusApplied:
		return nil
	case CanApply(co.Status):
	default:
		// Funds were captured for an amendment that was cancelled or reversed.
		// The credit must not be blocked (the money is real), but finance has to
		// see it and return the funds.
		amount := order.AmountCents
		s.raiseInTx(ctx, tx, tenantID, alert.Input{
			ProjectID:   &co.ProjectID,
			SubjectType: "change_order",
			SubjectID:   co.ID.String(),
			Code:        "change_order_paid_after_cancel",
			Severity:    alert.SeverityCritical,
			DedupeKey:   "change_order_paid_after_cancel:" + co.ID.String() + ":" + order.ID.String(),
			Title:       "Supplement payment captured for a cancelled change order",
			Detail: map[string]any{
				"project_id": co.ProjectID, "change_order_id": co.ID, "order_id": order.ID,
				"status": co.Status, "amount_cents": order.AmountCents, "currency": order.Currency,
			},
			AmountCents: &amount,
		})
		return nil
	}

	row := tx.QueryRow(ctx, `
		UPDATE change_orders SET status = 'applied', applied_at = now(), payment_order_id = $3,
			version = version + 1, updated_at = now()
		WHERE id = $1 AND tenant_id = $2
		RETURNING `+columns, co.ID, tenantID, order.ID)
	applied, err := scanChangeOrder(row)
	if err != nil {
		return fmt.Errorf("apply change order: %w", err)
	}
	if err := recomputeProjectChangeAmount(ctx, tx, tenantID, applied.ProjectID); err != nil {
		return err
	}
	detail := map[string]any{
		"project_id": applied.ProjectID, "order_no": applied.OrderNo,
		"payment_order_id": order.ID, "amount_cents": applied.AmountCents,
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "change_order.applied", "change_order", applied.ID.String(), "", detail); err != nil {
		return err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "change_order", applied.ID.String(), "change_order.applied", detail); err != nil {
		return err
	}
	return nil
}

// RefundSettled implements payment.RefundHook. A full refund of the supplement
// payment reverses the amendment; a partial refund keeps it applied and raises
// a warning so finance decides whether to re-bill.
func (s *Service) RefundSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p *payment.Payment, refund *payment.Refund) error {
	if p == nil || refund == nil {
		return nil
	}
	var changeOrderID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT change_order_id FROM payment_orders
		WHERE id = $1 AND tenant_id = $2 AND kind = 'change' AND change_order_id IS NOT NULL`,
		p.OrderID, tenantID).Scan(&changeOrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("locate refunded change order: %w", err)
	}
	co, err := lockChangeOrder(ctx, tx, tenantID, changeOrderID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !CanReverse(co.Status) {
		return nil
	}

	var refunded int64
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(sum(amount_cents), 0) FROM refunds
		WHERE tenant_id = $1 AND payment_id = $2 AND status = 'succeeded'`,
		tenantID, p.ID).Scan(&refunded); err != nil {
		return fmt.Errorf("sum supplement refunds: %w", err)
	}
	if refunded < p.AmountCents {
		amount := refund.AmountCents
		s.raiseInTx(ctx, tx, tenantID, alert.Input{
			ProjectID:   &co.ProjectID,
			SubjectType: "change_order",
			SubjectID:   co.ID.String(),
			Code:        "change_order_partially_refunded",
			Severity:    alert.SeverityWarning,
			DedupeKey:   "change_order_partial_refund:" + co.ID.String() + ":" + refund.ID.String(),
			Title:       "Supplement payment for an applied change order was partially refunded",
			Detail: map[string]any{
				"project_id": co.ProjectID, "change_order_id": co.ID,
				"payment_id": p.ID, "refund_id": refund.ID,
				"refunded_cents": refunded, "payment_cents": p.AmountCents,
			},
			AmountCents: &amount,
		})
		return nil
	}

	reason := strings.TrimSpace(refund.Reason)
	if reason == "" {
		reason = "supplement payment fully refunded"
	}
	row := tx.QueryRow(ctx, `
		UPDATE change_orders SET status = 'reversed', void_reason = $3,
			version = version + 1, updated_at = now()
		WHERE id = $1 AND tenant_id = $2
		RETURNING `+columns, co.ID, tenantID, reason)
	reversed, err := scanChangeOrder(row)
	if err != nil {
		return fmt.Errorf("reverse change order: %w", err)
	}
	if err := recomputeProjectChangeAmount(ctx, tx, tenantID, reversed.ProjectID); err != nil {
		return err
	}
	detail := map[string]any{
		"project_id": reversed.ProjectID, "order_no": reversed.OrderNo,
		"payment_id": p.ID, "refund_id": refund.ID, "amount_cents": reversed.AmountCents,
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "change_order.reversed", "change_order", reversed.ID.String(), "", detail); err != nil {
		return err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "change_order", reversed.ID.String(), "change_order.reversed", detail); err != nil {
		return err
	}
	return nil
}

func recomputeProjectChangeAmount(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE projects SET change_amount_cents = coalesce((
			SELECT sum(amount_cents) FROM change_orders
			WHERE tenant_id = $1 AND project_id = $2 AND status = 'applied'), 0),
			updated_at = now()
		WHERE id = $2 AND tenant_id = $1`, tenantID, projectID); err != nil {
		return fmt.Errorf("project change amount: %w", err)
	}
	return nil
}

func (s *Service) Get(ctx context.Context, tenantID, changeOrderID uuid.UUID) (*ChangeOrder, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ChangeOrder, error) {
		co, err := loadChangeOrder(ctx, tx, tenantID, changeOrderID)
		if err != nil {
			return nil, err
		}
		if co.Items, err = loadItems(ctx, tx, tenantID, co.ID); err != nil {
			return nil, err
		}
		return co, nil
	})
}

func (s *Service) List(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status string, limit, offset int) ([]ChangeOrder, int, error) {
	if status != "" && !ValidStatus(status) {
		return nil, 0, fmt.Errorf("%w: unknown status %q", ErrInvalidInput, status)
	}
	var (
		items []ChangeOrder
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const filter = `tenant_id = $1 AND ($2::uuid IS NULL OR project_id = $2)
			AND ($3::text = '' OR status = $3::text)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM change_orders WHERE `+filter,
			tenantID, projectID, status).Scan(&total); err != nil {
			return fmt.Errorf("count change orders: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM change_orders WHERE `+filter+`
			ORDER BY created_at DESC LIMIT $4 OFFSET $5`, tenantID, projectID, status, limit, offset)
		if err != nil {
			return fmt.Errorf("list change orders: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			co, err := scanChangeOrder(rows)
			if err != nil {
				return err
			}
			items = append(items, *co)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = []ChangeOrder{}
	}
	return items, total, nil
}

func (s *Service) get(ctx context.Context, tenantID, changeOrderID uuid.UUID) (*ChangeOrder, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*ChangeOrder, error) {
		return loadChangeOrder(ctx, tx, tenantID, changeOrderID)
	})
}

func (s *Service) livePaymentOrder(ctx context.Context, tenantID, changeOrderID uuid.UUID) (*uuid.UUID, error) {
	var orderID uuid.UUID
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id FROM payment_orders
			WHERE tenant_id = $1 AND change_order_id = $2 AND status IN ('creating','pending')
			ORDER BY created_at DESC LIMIT 1`, tenantID, changeOrderID).Scan(&orderID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load live supplement order: %w", err)
	}
	return &orderID, nil
}

func (s *Service) raisePaymentFailure(ctx context.Context, tenantID uuid.UUID, co *ChangeOrder, cause error) {
	if s.alerts == nil {
		return
	}
	amount := co.AmountCents
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
			ProjectID:   &co.ProjectID,
			SubjectType: "change_order",
			SubjectID:   co.ID.String(),
			Code:        "change_order_payment_failed",
			Severity:    alert.SeverityCritical,
			DedupeKey:   "change_order_payment_failed:" + co.ID.String(),
			Title:       "Signed change order could not open its supplement payment",
			Detail: map[string]any{
				"project_id": co.ProjectID, "change_order_id": co.ID,
				"order_no": co.OrderNo, "error": cause.Error(),
			},
			AmountCents: &amount,
		})
		return err
	})
	if err != nil {
		s.log.Error("raise change order payment alert", "change_order_id", co.ID, "err", err)
	}
}

func (s *Service) raiseInTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, in alert.Input) {
	if s.alerts == nil {
		return
	}
	if _, err := s.alerts.Raise(ctx, tx, tenantID, in); err != nil {
		s.log.Error("raise change order alert", "code", in.Code, "err", err)
	}
}

func loadChangeOrder(ctx context.Context, tx pgx.Tx, tenantID, changeOrderID uuid.UUID) (*ChangeOrder, error) {
	co, err := scanChangeOrder(tx.QueryRow(ctx, `
		SELECT `+columns+` FROM change_orders WHERE id = $1 AND tenant_id = $2`,
		changeOrderID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load change order: %w", err)
	}
	return co, nil
}

func lockChangeOrder(ctx context.Context, tx pgx.Tx, tenantID, changeOrderID uuid.UUID) (*ChangeOrder, error) {
	co, err := scanChangeOrder(tx.QueryRow(ctx, `
		SELECT `+columns+` FROM change_orders WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		changeOrderID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock change order: %w", err)
	}
	return co, nil
}

func loadItems(ctx context.Context, tx pgx.Tx, tenantID, changeOrderID uuid.UUID) ([]Item, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+itemColumns+` FROM change_order_items
		WHERE tenant_id = $1 AND change_order_id = $2 ORDER BY sort, id`,
		tenantID, changeOrderID)
	if err != nil {
		return nil, fmt.Errorf("list change order items: %w", err)
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ChangeOrderID, &it.Kind, &it.Description,
			&it.Quantity, &it.UnitPriceCents, &it.AmountCents, &it.Sort); err != nil {
			return nil, fmt.Errorf("scan change order item: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func scanChangeOrder(row rowScanner) (*ChangeOrder, error) {
	var co ChangeOrder
	if err := row.Scan(&co.ID, &co.ProjectID, &co.OrderNo, &co.Status, &co.Currency,
		&co.AmountCents, &co.Reason, &co.Snapshot, &co.SignTaskID, &co.SignURL,
		&co.SignerName, &co.SignerContact, &co.SignedDocumentHash, &co.SignedAt,
		&co.PaymentOrderID, &co.AppliedAt, &co.CancelledAt, &co.VoidReason,
		&co.Version, &co.CreatedBy, &co.CreatedAt, &co.UpdatedAt); err != nil {
		return nil, err
	}
	if len(co.Snapshot) == 0 {
		co.Snapshot = json.RawMessage(`{}`)
	}
	return &co, nil
}

// newOrderNo produces a human-readable, tenant-unique reference. Uniqueness is
// enforced by the index; the random suffix makes collisions negligible.
func newOrderNo() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return "CO-" + time.Now().UTC().Format("20060102") + "-" + strings.ToUpper(hex.EncodeToString(b)), nil
}

func callerActor(actor uuid.UUID) *uuid.UUID {
	if actor == uuid.Nil {
		return nil
	}
	return &actor
}

type rowScanner interface {
	Scan(dest ...any) error
}
