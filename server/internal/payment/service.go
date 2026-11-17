package payment

import (
	"context"
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
	"photography-workflow/server/internal/httpx"
	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/outbox"
)

var (
	ErrNotFound               = errors.New("payment not found")
	ErrProjectNotFound        = errors.New("project not found")
	ErrProjectClosed          = errors.New("project is cancelled or archived")
	ErrInvalidInput           = errors.New("invalid payment input")
	ErrDepositUnpaid          = errors.New("deposit must be paid before the balance")
	ErrAlreadyPaid            = errors.New("amount has already been paid")
	ErrOrderNotPending        = errors.New("payment order is not pending")
	ErrOrderInProgress        = errors.New("a payment order for this project and kind is being created")
	ErrOrderPaidAtProvider    = errors.New("the payment channel already captured this order")
	ErrReconcileMismatch      = errors.New("provider state does not match the local order")
	ErrRefundExceedsRemaining = errors.New("refund exceeds the remaining refundable amount")
	ErrUnknownProvider        = errors.New("unknown payment provider")
	ErrProvider               = errors.New("payment provider error")
)

// OrderCreateGrace is how long a "creating" reservation may stay unresolved
// before a retry (or the worker sweeper) closes it at the channel and starts
// over. Provider calls run outside transactions, so a crashed request can
// leave the row behind.
const OrderCreateGrace = 30 * time.Second

const orderColumns = `id, project_id, contract_id, kind, change_order_id, selection_charge_batch_id, provider, channel, currency,
	amount_cents, status, out_trade_no, provider_order_id, pay_url, expires_at, created_at, updated_at`

type Order struct {
	ID                     uuid.UUID  `json:"id"`
	ProjectID              uuid.UUID  `json:"project_id"`
	ContractID             *uuid.UUID `json:"contract_id,omitempty"`
	Kind                   string     `json:"kind"`
	ChangeOrderID          *uuid.UUID `json:"change_order_id,omitempty"`
	SelectionChargeBatchID *uuid.UUID `json:"selection_charge_batch_id,omitempty"`
	Provider               string     `json:"provider"`
	Channel                string     `json:"channel"`
	Currency               string     `json:"currency"`
	AmountCents            int64      `json:"amount_cents"`
	Status                 string     `json:"status"`
	OutTradeNo             string     `json:"out_trade_no"`
	ProviderOrderID        string     `json:"provider_order_id,omitempty"`
	PayURL                 string     `json:"pay_url"`
	ExpiresAt              time.Time  `json:"expires_at"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	// Derived from the project projection after a state change; empty on lists.
	ProjectPaymentStatus string `json:"project_payment_status,omitempty"`
}

const paymentColumns = `id, project_id, order_id, provider, channel, out_trade_no,
	provider_trade_no, amount_cents, currency, paid_at, created_at`

type Payment struct {
	ID              uuid.UUID `json:"id"`
	ProjectID       uuid.UUID `json:"project_id"`
	OrderID         uuid.UUID `json:"order_id"`
	Provider        string    `json:"provider"`
	Channel         string    `json:"channel"`
	OutTradeNo      string    `json:"out_trade_no"`
	ProviderTradeNo string    `json:"provider_trade_no,omitempty"`
	AmountCents     int64     `json:"amount_cents"`
	Currency        string    `json:"currency"`
	PaidAt          time.Time `json:"paid_at"`
	CreatedAt       time.Time `json:"created_at"`
}

const refundColumns = `id, project_id, payment_id, provider, out_refund_no,
	provider_refund_id, amount_cents, currency, reason, status, failure_reason,
	created_at, updated_at`

type Refund struct {
	ID               uuid.UUID `json:"id"`
	ProjectID        uuid.UUID `json:"project_id"`
	PaymentID        uuid.UUID `json:"payment_id"`
	Provider         string    `json:"provider"`
	OutRefundNo      string    `json:"out_refund_no"`
	ProviderRefundID string    `json:"provider_refund_id,omitempty"`
	AmountCents      int64     `json:"amount_cents"`
	Currency         string    `json:"currency"`
	Reason           string    `json:"reason"`
	Status           string    `json:"status"`
	FailureReason    string    `json:"failure_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// CallbackResult tells the webhook handler what happened. "rejected" means the
// event was authentic but refused (amount/currency mismatch); it is committed
// to the event log and audit trail before the handler returns an error.
type CallbackResult struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// OrderHook runs in the settlement transaction after an order is credited and
// before the project projection is refreshed. A hook error rolls the whole
// callback back, so a payment is never credited without its side effects.
type OrderHook interface {
	OrderSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, order *Order) error
}

// RefundHook mirrors OrderHook for a successfully settled refund.
type RefundHook interface {
	RefundSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, payment *Payment, refund *Refund) error
}

type Service struct {
	pool        *pgxpool.Pool
	provider    Provider
	orderTTL    time.Duration
	log         *slog.Logger
	alerts      *alert.Service
	orderHooks  []OrderHook
	refundHooks []RefundHook
}

func NewService(pool *pgxpool.Pool, provider Provider, orderTTL time.Duration, log *slog.Logger) *Service {
	if orderTTL <= 0 {
		orderTTL = 30 * time.Minute
	}
	return &Service{pool: pool, provider: provider, orderTTL: orderTTL, log: log}
}

// SetAlerts attaches the security alert sink. Without it, security findings
// are only audited.
func (s *Service) SetAlerts(a *alert.Service) { s.alerts = a }

// SetHooks attaches business services that react to settlement. Hooks must be
// transactional (they write through the passed tx) and idempotent.
func (s *Service) SetHooks(hooks ...any) {
	for _, h := range hooks {
		// A hook may implement both interfaces; register each capability
		// independently instead of letting the first match win.
		if v, ok := h.(OrderHook); ok {
			s.orderHooks = append(s.orderHooks, v)
		}
		if v, ok := h.(RefundHook); ok {
			s.refundHooks = append(s.refundHooks, v)
		}
	}
}

func (s *Service) notifyOrderSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, order *Order) error {
	for _, hook := range s.orderHooks {
		if err := hook.OrderSettled(ctx, tx, tenantID, order); err != nil {
			return fmt.Errorf("order settled hook: %w", err)
		}
	}
	return nil
}

func (s *Service) notifyRefundSettled(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, payment *Payment, refund *Refund) error {
	for _, hook := range s.refundHooks {
		if err := hook.RefundSettled(ctx, tx, tenantID, payment, refund); err != nil {
			return fmt.Errorf("refund settled hook: %w", err)
		}
	}
	return nil
}

type CreateOrderInput struct {
	ProjectID              uuid.UUID
	Kind                   string
	ChangeOrderID          *uuid.UUID
	SelectionChargeBatchID *uuid.UUID
}

type projectLedger struct {
	Title         string
	State         string
	Currency      string
	TotalCents    int64
	DepositCents  int64
	ChangeCents   int64
	PaymentStatus string
	NetCents      int64
	CoreNetCents  int64
}

// CreateOrder reserves the local row before calling the provider, so a channel
// order can never exist without a local record: the reservation carries the
// out_trade_no, and a crashed request is recovered (query/close) on retry.
func (s *Service) CreateOrder(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateOrderInput) (*Order, bool, error) {
	if !ValidKind(in.Kind) {
		return nil, false, fmt.Errorf("%w: kind must be deposit, balance, full or change", ErrInvalidInput)
	}
	if in.ProjectID == uuid.Nil {
		return nil, false, fmt.Errorf("%w: project_id is required", ErrInvalidInput)
	}
	switch {
	case in.Kind == KindChange && in.ChangeOrderID == nil:
		return nil, false, fmt.Errorf("%w: change_order_id is required for a change order", ErrInvalidInput)
	case in.Kind == KindSelectionExtra && in.SelectionChargeBatchID == nil:
		return nil, false, fmt.Errorf("%w: selection_charge_batch_id is required for a selection charge", ErrInvalidInput)
	case in.Kind != KindChange && in.ChangeOrderID != nil:
		return nil, false, fmt.Errorf("%w: change_order_id is only valid for change orders", ErrInvalidInput)
	case in.Kind != KindSelectionExtra && in.SelectionChargeBatchID != nil:
		return nil, false, fmt.Errorf("%w: selection_charge_batch_id is only valid for selection charges", ErrInvalidInput)
	}

	type plan struct {
		reuse    *Order
		stale    *Order
		amount   int64
		currency string
	}
	var p plan
	// Phase 1: validate against current state and decide whether an order can be
	// reused or must be closed at the channel first.
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		ledger, err := loadLedger(ctx, tx, tenantID, in.ProjectID, false)
		if err != nil {
			return err
		}
		live, err := liveOrder(ctx, tx, tenantID, in.ProjectID, in.Kind, in.ChangeOrderID, in.SelectionChargeBatchID, false)
		if err != nil {
			return err
		}
		if live == nil {
			return nil
		}
		amount, err := orderAmountFor(ctx, tx, tenantID, in, ledger, false)
		if err != nil {
			return err
		}
		p.amount, p.currency = amount, ledger.Currency
		if reusableOrder(live, amount, ledger.Currency) {
			live.ProjectPaymentStatus = ledger.PaymentStatus
			p.reuse = live
			return nil
		}
		if live.Status == OrderCreating && time.Since(live.CreatedAt) < OrderCreateGrace {
			return ErrOrderInProgress
		}
		p.stale = live
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if p.reuse != nil {
		// The local row says pending, but only the channel knows whether the
		// payment was captured while the callback was lost.
		state, qerr := s.provider.QueryOrder(ctx, p.reuse.OutTradeNo)
		switch {
		case qerr == nil && state.Status == OrderPaid:
			order, rerr := s.ReconcileOrder(ctx, tenantID, p.reuse.ID)
			if rerr != nil {
				return nil, false, rerr
			}
			return order, false, nil
		case qerr == nil && state.Status == OrderPending:
			return p.reuse, false, nil
		case errors.Is(qerr, ErrProviderOrderNotFound), qerr == nil:
			// The channel closed or forgot the order: close locally and start over.
			p.stale, p.reuse = p.reuse, nil
		case qerr != nil:
			// A channel outage must not block a still-valid pay link.
			s.log.Warn("query order on reuse", "order_id", p.reuse.ID, "err", qerr)
			return p.reuse, false, nil
		}
	}

	// Phase 2: close the stale channel order before asking for a new one. A
	// captured payment is reconciled instead of duplicated.
	if p.stale != nil {
		switch err := s.provider.CloseOrder(ctx, p.stale.OutTradeNo); {
		case errors.Is(err, ErrProviderOrderPaid):
			order, rerr := s.ReconcileOrder(ctx, tenantID, p.stale.ID)
			if rerr != nil {
				return nil, false, rerr
			}
			if order.Status != OrderPaid {
				return nil, false, fmt.Errorf("%w: channel reported paid but reconcile left status %s", ErrProvider, order.Status)
			}
			return order, false, nil
		case err != nil && !errors.Is(err, ErrProviderOrderNotFound):
			return nil, false, fmt.Errorf("%w: close stale order: %v", ErrProvider, err)
		}
	}

	outTradeNo, err := newTradeNo("PWF")
	if err != nil {
		return nil, false, err
	}

	// Phase 3: reserve the order and its callback route in one transaction.
	type createResult struct {
		Order   *Order
		Created bool
	}
	res, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (createResult, error) {
		ledger, err := loadLedger(ctx, tx, tenantID, in.ProjectID, true)
		if err != nil {
			return createResult{}, err
		}
		live, err := liveOrder(ctx, tx, tenantID, in.ProjectID, in.Kind, in.ChangeOrderID, in.SelectionChargeBatchID, true)
		if err != nil {
			return createResult{}, err
		}
		amount, err := orderAmountFor(ctx, tx, tenantID, in, ledger, true)
		if err != nil {
			return createResult{}, err
		}
		if live != nil {
			if live.Status == OrderPaid {
				live.ProjectPaymentStatus = ledger.PaymentStatus
				return createResult{Order: live}, nil
			}
			switch {
			case p.stale != nil && live.ID == p.stale.ID:
				// The channel order was closed in phase 2; finish the local close.
				tag, err := tx.Exec(ctx, `
					UPDATE payment_orders SET status = 'closed', updated_at = now()
					WHERE id = $1 AND tenant_id = $2 AND status IN ('creating','pending')`,
					live.ID, tenantID)
				if err != nil {
					return createResult{}, fmt.Errorf("close stale payment order: %w", err)
				}
				if tag.RowsAffected() == 0 {
					return createResult{}, ErrOrderInProgress
				}
				if _, err := refreshProjectPaymentStatus(ctx, tx, tenantID, in.ProjectID); err != nil {
					return createResult{}, err
				}
				detail := map[string]any{
					"project_id": in.ProjectID, "out_trade_no": live.OutTradeNo, "reason": "stale",
				}
				if err := audit.Write(ctx, tx, tenantID, callerActor(actor), "payment.order_closed", "payment_order", live.ID.String(), ip, detail); err != nil {
					return createResult{}, err
				}
				if err := outbox.Emit(ctx, tx, tenantID, "payment_order", live.ID.String(), "payment.order_closed", detail); err != nil {
					return createResult{}, err
				}
			case reusableOrder(live, amount, ledger.Currency):
				live.ProjectPaymentStatus = ledger.PaymentStatus
				return createResult{Order: live}, nil
			default:
				return createResult{}, ErrOrderInProgress
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO payment_orders (tenant_id, project_id, contract_id, kind, change_order_id, selection_charge_batch_id,
				provider, channel, currency, amount_cents, status, out_trade_no, expires_at, created_by)
			VALUES ($1,$2,NULL,$3,$4,$5,$6,$7,$8,$9,'creating',$10,$11,$12)
			RETURNING `+orderColumns,
			tenantID, in.ProjectID, in.Kind, in.ChangeOrderID, in.SelectionChargeBatchID, s.provider.Name(), s.provider.Name(),
			ledger.Currency, amount, outTradeNo, time.Now().Add(s.orderTTL), callerActor(actor))
		reserved, err := scanOrder(row)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return createResult{}, ErrOrderInProgress
			}
			return createResult{}, fmt.Errorf("reserve payment order: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_order_routes (provider, out_trade_no, tenant_id, order_id)
			VALUES ($1,$2,$3,$4)`,
			s.provider.Name(), reserved.OutTradeNo, tenantID, reserved.ID); err != nil {
			return createResult{}, fmt.Errorf("record order route: %w", err)
		}
		return createResult{Order: reserved, Created: true}, nil
	})
	if err != nil {
		return nil, false, err
	}

	// Phase 4: the provider call happens outside any transaction so vendor
	// latency never holds project locks.
	params, err := s.provider.CreateOrder(ctx, OrderRequest{
		OutTradeNo:  res.Order.OutTradeNo,
		Description: fmt.Sprintf("project %s %s", res.Order.ProjectID, res.Order.Kind),
		Currency:    res.Order.Currency,
		AmountCents: res.Order.AmountCents,
		ExpireAt:    time.Now().Add(s.orderTTL),
		ClientIP:    ip,
	})
	if err != nil {
		// A timeout may hide a successful creation, so try to close whatever
		// the channel has before abandoning the reservation.
		paidAtChannel := false
		switch cerr := s.provider.CloseOrder(ctx, res.Order.OutTradeNo); {
		case errors.Is(cerr, ErrProviderOrderPaid):
			paidAtChannel = true
			if order, rerr := s.ReconcileOrder(ctx, tenantID, res.Order.ID); rerr == nil {
				return order, false, nil
			} else {
				s.log.Error("reconcile order paid while create reported an error", "order_id", res.Order.ID, "err", rerr)
			}
		case cerr != nil && !errors.Is(cerr, ErrProviderOrderNotFound):
			s.log.Error("close channel order after create failure", "order_id", res.Order.ID, "err", cerr)
		}
		if paidAtChannel {
			// Keep the reservation so the next attempt or a manual reconcile can
			// credit the captured payment instead of hiding it behind a close.
			return nil, false, fmt.Errorf("%w: channel captured the order but reconcile failed", ErrProvider)
		}
		if aerr := s.abandonOrder(ctx, tenantID, res.Order.ID, ip); aerr != nil {
			s.log.Error("abandon reserved payment order", "order_id", res.Order.ID, "err", aerr)
		}
		return nil, false, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	if params == nil || params.PayURL == "" {
		if aerr := s.abandonOrder(ctx, tenantID, res.Order.ID, ip); aerr != nil {
			s.log.Error("abandon reserved payment order", "order_id", res.Order.ID, "err", aerr)
		}
		return nil, false, fmt.Errorf("%w: provider returned no pay url", ErrProvider)
	}

	// Phase 5: publish the pay URL and refresh the projection.
	order, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Order, error) {
		locked, err := lockOrder(ctx, tx, tenantID, res.Order.ID)
		if err != nil {
			return nil, err
		}
		if locked.Status == OrderPaid {
			return locked, nil
		}
		if locked.Status != OrderCreating {
			return nil, fmt.Errorf("%w: order is %s", ErrOrderNotPending, locked.Status)
		}
		row := tx.QueryRow(ctx, `
			UPDATE payment_orders SET status = 'pending', provider_order_id = $3, pay_url = $4,
				expires_at = $5, updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+orderColumns,
			res.Order.ID, tenantID, params.ProviderOrderID, params.PayURL, time.Now().Add(s.orderTTL))
		updated, err := scanOrder(row)
		if err != nil {
			return nil, fmt.Errorf("publish payment order: %w", err)
		}
		status, err := refreshProjectPaymentStatus(ctx, tx, tenantID, in.ProjectID)
		if err != nil {
			return nil, err
		}
		updated.ProjectPaymentStatus = status
		detail := map[string]any{
			"project_id": in.ProjectID, "kind": in.Kind, "amount_cents": updated.AmountCents,
			"out_trade_no": updated.OutTradeNo,
		}
		if err := audit.Write(ctx, tx, tenantID, callerActor(actor), "payment.order_created", "payment_order", updated.ID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "payment_order", updated.ID.String(), "payment.order_created", detail); err != nil {
			return nil, err
		}
		return updated, nil
	})
	if err != nil {
		return nil, false, err
	}
	return order, res.Created, nil
}

func (s *Service) abandonOrder(ctx context.Context, tenantID, orderID uuid.UUID, ip string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		order, err := lockOrder(ctx, tx, tenantID, orderID)
		if err != nil {
			return err
		}
		if order.Status != OrderCreating {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_orders SET status = 'closed', updated_at = now()
			WHERE id = $1 AND tenant_id = $2`, orderID, tenantID); err != nil {
			return fmt.Errorf("abandon payment order: %w", err)
		}
		if _, err := refreshProjectPaymentStatus(ctx, tx, tenantID, order.ProjectID); err != nil {
			return err
		}
		detail := map[string]any{
			"project_id": order.ProjectID, "out_trade_no": order.OutTradeNo, "reason": "provider_error",
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "payment.order_abandoned", "payment_order", orderID.String(), ip, detail); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, tenantID, "payment_order", orderID.String(), "payment.order_abandoned", detail)
	})
}

func (s *Service) GetOrder(ctx context.Context, tenantID, orderID uuid.UUID) (*Order, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Order, error) {
		order, err := scanOrder(tx.QueryRow(ctx, `
			SELECT `+orderColumns+` FROM payment_orders WHERE id = $1 AND tenant_id = $2`,
			orderID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get payment order: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT payment_status FROM projects WHERE tenant_id = $1 AND id = $2`,
			tenantID, order.ProjectID).Scan(&order.ProjectPaymentStatus); err != nil {
			return nil, fmt.Errorf("load payment order project status: %w", err)
		}
		return order, nil
	})
}

// CompleteSandboxOrder exercises the ordinary payment callback path without
// exposing callback signing material to a customer or development client.
func (s *Service) CompleteSandboxOrder(ctx context.Context, tenantID, orderID uuid.UUID) (*CallbackResult, error) {
	provider, ok := s.provider.(*Sandbox)
	if !ok {
		return nil, fmt.Errorf("%w: sandbox checkout is disabled for this provider", ErrInvalidInput)
	}
	order, err := s.GetOrder(ctx, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	state, err := provider.QueryOrder(ctx, order.OutTradeNo)
	if err == nil && state.Status == OrderPaid {
		if _, err := s.ReconcileOrder(ctx, tenantID, orderID); err != nil {
			return nil, err
		}
		return &CallbackResult{Status: "reconciled"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query sandbox order: %w", err)
	}
	ev, err := provider.CompleteOrder(order.OutTradeNo)
	if err != nil {
		return nil, err
	}
	return s.HandleCallback(ctx, provider.Name(), ev)
}

func (s *Service) projectPaymentStatus(ctx context.Context, tenantID, projectID uuid.UUID) (string, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (string, error) {
		var status string
		if err := tx.QueryRow(ctx, `
			SELECT payment_status FROM projects WHERE id = $1 AND tenant_id = $2`,
			projectID, tenantID).Scan(&status); err != nil {
			return "", fmt.Errorf("load project payment status: %w", err)
		}
		return status, nil
	})
}

func (s *Service) ListOrders(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, status, kind string, limit, offset int) ([]Order, int, error) {
	if status != "" && status != OrderCreating && status != OrderPending && status != OrderPaid && status != OrderClosed {
		return nil, 0, fmt.Errorf("%w: unknown order status %q", ErrInvalidInput, status)
	}
	if kind != "" && !ValidKind(kind) {
		return nil, 0, fmt.Errorf("%w: unknown kind %q", ErrInvalidInput, kind)
	}
	var (
		items []Order
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		const filter = `tenant_id = $1 AND ($2::uuid IS NULL OR project_id = $2)
			AND ($3::text = '' OR status = $3::text) AND ($4::text = '' OR kind = $4::text)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_orders WHERE `+filter,
			tenantID, projectID, status, kind).Scan(&total); err != nil {
			return fmt.Errorf("count payment orders: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+orderColumns+` FROM payment_orders WHERE `+filter+`
			ORDER BY created_at DESC LIMIT $5 OFFSET $6`, tenantID, projectID, status, kind, limit, offset)
		if err != nil {
			return fmt.Errorf("list payment orders: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanOrder(rows)
			if err != nil {
				return err
			}
			items = append(items, *o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// CloseOrder closes the order at the channel before the local row, so a pay
// link can never outlive the local state. If the channel already captured the
// payment the order is reconciled instead of closed.
func (s *Service) CloseOrder(ctx context.Context, tenantID, actor, orderID uuid.UUID, ip string) (*Order, error) {
	order, err := s.GetOrder(ctx, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	switch order.Status {
	case OrderClosed:
		return order, nil
	case OrderPaid:
		return nil, fmt.Errorf("%w: order is %s", ErrOrderNotPending, order.Status)
	}

	switch cerr := s.provider.CloseOrder(ctx, order.OutTradeNo); {
	case errors.Is(cerr, ErrProviderOrderPaid):
		if _, rerr := s.ReconcileOrder(ctx, tenantID, orderID); rerr != nil {
			return nil, rerr
		}
		amount := order.AmountCents
		s.raiseStandalone(ctx, tenantID, alert.Input{
			ProjectID:   &order.ProjectID,
			SubjectType: "payment_order",
			SubjectID:   order.ID.String(),
			Code:        "close_after_paid",
			Severity:    alert.SeverityWarning,
			DedupeKey:   "close_paid:" + order.ID.String(),
			Title:       "Order was paid while being closed at the channel",
			Detail: map[string]any{
				"project_id": order.ProjectID, "order_id": order.ID, "kind": order.Kind,
				"out_trade_no": order.OutTradeNo, "amount_cents": order.AmountCents,
				"currency": order.Currency,
			},
			AmountCents: &amount,
		})
		return nil, fmt.Errorf("%w: channel captured the order; reconciled as paid", ErrOrderPaidAtProvider)
	case cerr != nil && !errors.Is(cerr, ErrProviderOrderNotFound):
		return nil, fmt.Errorf("%w: close at channel: %v", ErrProvider, cerr)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Order, error) {
		locked, err := lockOrder(ctx, tx, tenantID, orderID)
		if err != nil {
			return nil, err
		}
		if locked.Status == OrderClosed {
			return locked, nil
		}
		if locked.Status != OrderPending && locked.Status != OrderCreating {
			return nil, fmt.Errorf("%w: order is %s", ErrOrderNotPending, locked.Status)
		}
		row := tx.QueryRow(ctx, `
			UPDATE payment_orders SET status = 'closed', updated_at = now()
			WHERE id = $1 AND tenant_id = $2 RETURNING `+orderColumns, orderID, tenantID)
		closed, err := scanOrder(row)
		if err != nil {
			return nil, fmt.Errorf("close payment order: %w", err)
		}
		status, err := refreshProjectPaymentStatus(ctx, tx, tenantID, locked.ProjectID)
		if err != nil {
			return nil, err
		}
		closed.ProjectPaymentStatus = status
		detail := map[string]any{"project_id": locked.ProjectID, "out_trade_no": locked.OutTradeNo}
		if err := audit.Write(ctx, tx, tenantID, &actor, "payment.order_closed", "payment_order", orderID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "payment_order", orderID.String(), "payment.order_closed", detail); err != nil {
			return nil, err
		}
		return closed, nil
	})
}

// ReconcileOrder asks the channel for its authoritative state and aligns the
// local order. It is the recovery path for lost callbacks: a channel-paid
// order is credited exactly once, a channel-closed order is closed locally.
func (s *Service) ReconcileOrder(ctx context.Context, tenantID, orderID uuid.UUID) (*Order, error) {
	order, err := s.GetOrder(ctx, tenantID, orderID)
	if err != nil {
		return nil, err
	}
	state, err := s.provider.QueryOrder(ctx, order.OutTradeNo)
	if errors.Is(err, ErrProviderOrderNotFound) {
		s.alertReconcileMismatch(ctx, tenantID, order, "channel_order_missing",
			"Payment channel has no record of the order")
		return nil, fmt.Errorf("%w: channel has no record of the order", ErrReconcileMismatch)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: query order: %v", ErrProvider, err)
	}

	switch state.Status {
	case OrderPaid:
		settled, err := s.settleChannelPayment(ctx, tenantID, order.ID, state)
		if errors.Is(err, ErrReconcileMismatch) {
			s.alertReconcileMismatch(ctx, tenantID, order, "settle_mismatch",
				"Channel payment does not match the local order")
		}
		return settled, err
	case OrderPending:
		if order.Status != OrderPending {
			s.alertReconcileMismatch(ctx, tenantID, order, "channel_pending_local_"+order.Status,
				"Channel still accepts payment but the local order is not pending")
			return nil, fmt.Errorf("%w: local order is %s but the channel still accepts payment", ErrReconcileMismatch, order.Status)
		}
		if order.ProjectPaymentStatus, err = s.projectPaymentStatus(ctx, tenantID, order.ProjectID); err != nil {
			return nil, err
		}
		return order, nil
	case OrderClosed:
		if order.Status == OrderPaid {
			s.alertReconcileMismatch(ctx, tenantID, order, "channel_closed_local_paid",
				"Channel reports closed for an order recorded as paid")
			return nil, fmt.Errorf("%w: local order is paid but the channel reports closed", ErrReconcileMismatch)
		}
		if order.Status == OrderClosed {
			return order, nil
		}
		return s.closeOrderLocal(ctx, tenantID, order.ID, "reconcile")
	default:
		s.alertReconcileMismatch(ctx, tenantID, order, "unknown_channel_status",
			"Payment channel returned an unknown order status")
		return nil, fmt.Errorf("%w: unknown channel status %q", ErrReconcileMismatch, state.Status)
	}
}

// alertReconcileMismatch surfaces a divergence between the channel and the
// local order. Reconciliation keeps its error contract; alerting is best-effort
// and runs in its own transaction.
func (s *Service) alertReconcileMismatch(ctx context.Context, tenantID uuid.UUID, order *Order, reason, title string) {
	amount := order.AmountCents
	s.raiseStandalone(ctx, tenantID, alert.Input{
		ProjectID:   &order.ProjectID,
		SubjectType: "payment_order",
		SubjectID:   order.ID.String(),
		Code:        "reconcile_mismatch",
		Severity:    alert.SeverityWarning,
		DedupeKey:   "reconcile:" + order.ID.String() + ":" + reason,
		Title:       title,
		Detail: map[string]any{
			"project_id": order.ProjectID, "order_id": order.ID, "kind": order.Kind,
			"order_status": order.Status, "out_trade_no": order.OutTradeNo,
			"amount_cents": order.AmountCents, "currency": order.Currency, "reason": reason,
		},
		AmountCents: &amount,
	})
}

// settleChannelPayment credits a channel-confirmed payment with a synthetic
// event id, so it stays unique and observable next to provider callbacks.
func (s *Service) settleChannelPayment(ctx context.Context, tenantID, orderID uuid.UUID, state *OrderState) (*Order, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Order, error) {
		order, err := lockOrder(ctx, tx, tenantID, orderID)
		if err != nil {
			return nil, err
		}
		if order.Status == OrderPaid {
			if err := tx.QueryRow(ctx, `
				SELECT payment_status FROM projects WHERE id = $1 AND tenant_id = $2`,
				order.ProjectID, tenantID).Scan(&order.ProjectPaymentStatus); err != nil {
				return nil, fmt.Errorf("load project payment status: %w", err)
			}
			return order, nil
		}
		if order.Status == OrderClosed {
			return nil, fmt.Errorf("%w: the channel captured a closed order", ErrReconcileMismatch)
		}
		if state.AmountCents != order.AmountCents || state.Currency != order.Currency {
			return nil, fmt.Errorf("%w: channel has %d %s, local order has %d %s",
				ErrReconcileMismatch, state.AmountCents, state.Currency, order.AmountCents, order.Currency)
		}

		eventID := "reconcile:" + order.OutTradeNo
		eventPayload, err := json.Marshal(map[string]any{
			"source": "reconcile", "out_trade_no": order.OutTradeNo,
			"provider_trade_no": state.ProviderTradeNo, "amount_cents": state.AmountCents,
			"currency": state.Currency, "paid_at": state.PaidAt,
		})
		if err != nil {
			return nil, fmt.Errorf("encode reconcile event: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_events (tenant_id, provider, event_id, event_type, ref_no, outcome, payload)
			VALUES ($1,$2,$3,$4,$5,'applied',$6)
			ON CONFLICT (provider, event_id) DO NOTHING`,
			tenantID, order.Provider, eventID, EventPaymentSucceeded, order.OutTradeNo, eventPayload); err != nil {
			return nil, fmt.Errorf("record reconcile event: %w", err)
		}

		paidAt := time.Now().UTC()
		if state.PaidAt != nil {
			paidAt = state.PaidAt.UTC()
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payments (tenant_id, project_id, order_id, provider, channel,
				out_trade_no, provider_trade_no, amount_cents, currency, paid_at, event_id, raw)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (order_id) DO NOTHING`,
			tenantID, order.ProjectID, order.ID, order.Provider, order.Channel,
			order.OutTradeNo, state.ProviderTradeNo, order.AmountCents, order.Currency, paidAt,
			eventID, eventPayload); err != nil {
			return nil, fmt.Errorf("record reconciled payment: %w", err)
		}
		row := tx.QueryRow(ctx, `
			UPDATE payment_orders SET status = 'paid',
				provider_order_id = CASE WHEN provider_order_id = '' AND $3 <> '' THEN $3 ELSE provider_order_id END,
				updated_at = now()
			WHERE id = $1 AND tenant_id = $2 RETURNING `+orderColumns,
			orderID, tenantID, state.ProviderTradeNo)
		settled, err := scanOrder(row)
		if err != nil {
			return nil, fmt.Errorf("mark reconciled order paid: %w", err)
		}
		if err := s.notifyOrderSettled(ctx, tx, tenantID, settled); err != nil {
			return nil, err
		}
		status, err := refreshProjectPaymentStatus(ctx, tx, tenantID, order.ProjectID)
		if err != nil {
			return nil, err
		}
		settled.ProjectPaymentStatus = status
		detail := map[string]any{
			"project_id": order.ProjectID, "order_id": order.ID, "kind": order.Kind,
			"amount_cents": order.AmountCents, "currency": order.Currency,
			"event_id": eventID, "provider_trade_no": state.ProviderTradeNo,
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "payment.reconciled", "payment", order.ID.String(), "", detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "payment", order.ID.String(), "payment.reconciled", detail); err != nil {
			return nil, err
		}
		return settled, nil
	})
}

func (s *Service) closeOrderLocal(ctx context.Context, tenantID, orderID uuid.UUID, reason string) (*Order, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Order, error) {
		order, err := lockOrder(ctx, tx, tenantID, orderID)
		if err != nil {
			return nil, err
		}
		if order.Status == OrderClosed {
			return order, nil
		}
		if order.Status != OrderPending && order.Status != OrderCreating {
			return nil, fmt.Errorf("%w: order is %s", ErrOrderNotPending, order.Status)
		}
		row := tx.QueryRow(ctx, `
			UPDATE payment_orders SET status = 'closed', updated_at = now()
			WHERE id = $1 AND tenant_id = $2 RETURNING `+orderColumns, orderID, tenantID)
		closed, err := scanOrder(row)
		if err != nil {
			return nil, fmt.Errorf("close payment order: %w", err)
		}
		status, err := refreshProjectPaymentStatus(ctx, tx, tenantID, order.ProjectID)
		if err != nil {
			return nil, err
		}
		closed.ProjectPaymentStatus = status
		detail := map[string]any{
			"project_id": order.ProjectID, "out_trade_no": order.OutTradeNo, "reason": reason,
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "payment.order_closed", "payment_order", orderID.String(), "", detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "payment_order", orderID.String(), "payment.order_closed", detail); err != nil {
			return nil, err
		}
		return closed, nil
	})
}

func (s *Service) ListPayments(ctx context.Context, tenantID uuid.UUID, projectID *uuid.UUID, limit, offset int) ([]Payment, int, error) {
	var (
		items []Payment
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		filter := `tenant_id = $1 AND ($2::uuid IS NULL OR project_id = $2)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payments WHERE `+filter,
			tenantID, projectID).Scan(&total); err != nil {
			return fmt.Errorf("count payments: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+paymentColumns+` FROM payments WHERE `+filter+`
			ORDER BY paid_at DESC LIMIT $3 OFFSET $4`, tenantID, projectID, limit, offset)
		if err != nil {
			return fmt.Errorf("list payments: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPayment(rows)
			if err != nil {
				return err
			}
			items = append(items, *p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Service) GetPayment(ctx context.Context, tenantID, paymentID uuid.UUID) (*Payment, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Payment, error) {
		p, err := scanPayment(tx.QueryRow(ctx, `
			SELECT `+paymentColumns+` FROM payments WHERE id = $1 AND tenant_id = $2`,
			paymentID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get payment: %w", err)
		}
		return p, nil
	})
}

type CreateRefundInput struct {
	PaymentID   uuid.UUID
	AmountCents int64
	Reason      string
}

func (s *Service) CreateRefund(ctx context.Context, tenantID, actor uuid.UUID, ip string, in CreateRefundInput) (*Refund, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || len(reason) > 500 {
		return nil, fmt.Errorf("%w: reason is required and must be at most 500 chars", ErrInvalidInput)
	}

	var paymentTradeNo string
	refund, err := db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Refund, error) {
		payment, err := lockPayment(ctx, tx, tenantID, in.PaymentID)
		if err != nil {
			return nil, err
		}
		paymentTradeNo = payment.OutTradeNo
		remaining, err := refundableAmount(ctx, tx, tenantID, payment)
		if err != nil {
			return nil, err
		}
		amount := in.AmountCents
		if amount <= 0 {
			amount = remaining
		}
		if remaining <= 0 {
			return nil, fmt.Errorf("%w: nothing left to refund", ErrRefundExceedsRemaining)
		}
		if amount > remaining {
			return nil, fmt.Errorf("%w: %d requested, %d refundable", ErrRefundExceedsRemaining, amount, remaining)
		}
		outRefundNo, err := newTradeNo("RFD")
		if err != nil {
			return nil, err
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO refunds (tenant_id, project_id, payment_id, provider, out_refund_no,
				amount_cents, currency, reason, status, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9)
			RETURNING `+refundColumns,
			tenantID, payment.ProjectID, payment.ID, s.provider.Name(), outRefundNo,
			amount, payment.Currency, reason, actor)
		refund, err := scanRefund(row)
		if err != nil {
			return nil, fmt.Errorf("create refund: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_refund_routes (provider, out_refund_no, tenant_id, refund_id)
			VALUES ($1,$2,$3,$4)`,
			s.provider.Name(), refund.OutRefundNo, tenantID, refund.ID); err != nil {
			return nil, fmt.Errorf("record refund route: %w", err)
		}
		detail := map[string]any{
			"project_id": payment.ProjectID, "payment_id": payment.ID,
			"amount_cents": amount, "out_refund_no": refund.OutRefundNo,
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "payment.refund_requested", "refund", refund.ID.String(), ip, detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "refund", refund.ID.String(), "payment.refund_requested", detail); err != nil {
			return nil, err
		}
		return refund, nil
	})
	if err != nil {
		return nil, err
	}

	// The refund row exists first so a provider failure is recorded rather
	// than losing the attempt.
	result, err := s.provider.Refund(ctx, RefundRequest{
		OutTradeNo:  paymentTradeNo,
		OutRefundNo: refund.OutRefundNo,
		Reason:      refund.Reason,
		Currency:    refund.Currency,
		AmountCents: refund.AmountCents,
	})
	if err != nil {
		if ferr := s.markRefundFailed(ctx, tenantID, refund.ID, err.Error()); ferr != nil {
			s.log.Error("mark refund failed", "refund_id", refund.ID, "err", ferr)
		}
		return nil, fmt.Errorf("%w: %v", ErrProvider, err)
	}

	if result.Status == RefundSucceeded {
		settled, err := s.settleRefund(ctx, tenantID, refund.ID, result.ProviderRefundID, nil)
		if err != nil {
			return nil, err
		}
		return settled, nil
	}
	if err := s.attachProviderRefund(ctx, tenantID, refund.ID, result.ProviderRefundID); err != nil {
		return nil, err
	}
	return s.GetRefund(ctx, tenantID, refund.ID)
}

func (s *Service) attachProviderRefund(ctx context.Context, tenantID, refundID uuid.UUID, providerRefundID string) error {
	if providerRefundID == "" {
		return nil
	}
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE refunds SET provider_refund_id = $3, updated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND status = 'pending'`,
			refundID, tenantID, providerRefundID); err != nil {
			return fmt.Errorf("record provider refund id: %w", err)
		}
		return nil
	})
}

func (s *Service) GetRefund(ctx context.Context, tenantID, refundID uuid.UUID) (*Refund, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Refund, error) {
		r, err := scanRefund(tx.QueryRow(ctx, `
			SELECT `+refundColumns+` FROM refunds WHERE id = $1 AND tenant_id = $2`,
			refundID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get refund: %w", err)
		}
		return r, nil
	})
}

func (s *Service) ListRefunds(ctx context.Context, tenantID uuid.UUID, paymentID *uuid.UUID, limit, offset int) ([]Refund, int, error) {
	var (
		items []Refund
		total int
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		filter := `tenant_id = $1 AND ($2::uuid IS NULL OR payment_id = $2)`
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM refunds WHERE `+filter,
			tenantID, paymentID).Scan(&total); err != nil {
			return fmt.Errorf("count refunds: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT `+refundColumns+` FROM refunds WHERE `+filter+`
			ORDER BY created_at DESC LIMIT $3 OFFSET $4`, tenantID, paymentID, limit, offset)
		if err != nil {
			return fmt.Errorf("list refunds: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRefund(rows)
			if err != nil {
				return err
			}
			items = append(items, *r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// HandleCallback resolves the tenant from the provider reference, then applies
// the event inside a tenant-scoped transaction. Events are unique per provider
// so redeliveries can never credit twice.
func (s *Service) HandleCallback(ctx context.Context, providerName string, ev *CallbackEvent) (*CallbackResult, error) {
	if providerName != s.provider.Name() {
		return nil, ErrUnknownProvider
	}
	switch ev.Type {
	case EventPaymentSucceeded:
		return s.handlePaymentCallback(ctx, providerName, ev)
	case EventRefundSucceeded, EventRefundFailed:
		return s.handleRefundCallback(ctx, providerName, ev)
	default:
		return &CallbackResult{Status: "ignored"}, nil
	}
}

func (s *Service) handlePaymentCallback(ctx context.Context, providerName string, ev *CallbackEvent) (*CallbackResult, error) {
	var (
		tenantID uuid.UUID
		orderID  uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, order_id FROM payment_order_routes
		WHERE provider = $1 AND out_trade_no = $2`, providerName, ev.OutTradeNo).Scan(&tenantID, &orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &CallbackResult{Status: "ignored"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup payment order route: %w", err)
	}

	payload := ev.Raw
	if !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*CallbackResult, error) {
		order, err := lockOrder(ctx, tx, tenantID, orderID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return &CallbackResult{Status: "ignored"}, nil
			}
			return nil, err
		}
		// Lock the project before the order-dependent writes so every payment
		// path serialises on the same aggregate first.
		if _, err := lockProject(ctx, tx, tenantID, order.ProjectID); err != nil {
			return nil, err
		}

		fresh, err := recordEvent(ctx, tx, tenantID, providerName, ev, payload, "applied")
		if err != nil {
			return nil, err
		}
		if !fresh {
			return &CallbackResult{Status: "duplicate"}, nil
		}
		if order.Status == OrderPaid {
			if err := setEventOutcome(ctx, tx, providerName, ev.EventID, "duplicate"); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "duplicate"}, nil
		}
		if order.Status != OrderPending {
			// The order was closed locally, yet the customer still paid. Never
			// credit silently: flag it for refund/manual resolution instead.
			if err := s.flagLatePayment(ctx, tx, tenantID, providerName, ev, order); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "ignored", Reason: "late_payment"}, nil
		}
		if ev.AmountCents != order.AmountCents {
			if err := s.rejectEvent(ctx, tx, tenantID, providerName, ev, order.ProjectID, "amount_mismatch", payload); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "rejected", Reason: "amount_mismatch"}, nil
		}
		if ev.Currency != order.Currency {
			if err := s.rejectEvent(ctx, tx, tenantID, providerName, ev, order.ProjectID, "currency_mismatch", payload); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "rejected", Reason: "currency_mismatch"}, nil
		}

		paidAt := time.Now().UTC()
		if ev.PaidAt != nil {
			paidAt = ev.PaidAt.UTC()
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO payments (tenant_id, project_id, order_id, provider, channel,
				out_trade_no, provider_trade_no, amount_cents, currency, paid_at, event_id, raw)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (order_id) DO NOTHING`,
			tenantID, order.ProjectID, order.ID, providerName, order.Channel,
			order.OutTradeNo, ev.ProviderTradeNo, ev.AmountCents, ev.Currency, paidAt,
			ev.EventID, payload)
		if err != nil {
			return nil, fmt.Errorf("record payment: %w", err)
		}
		if tag.RowsAffected() == 0 {
			if err := setEventOutcome(ctx, tx, providerName, ev.EventID, "duplicate"); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "duplicate"}, nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_orders SET status = 'paid',
				provider_order_id = CASE WHEN provider_order_id = '' AND $3 <> '' THEN $3 ELSE provider_order_id END,
				updated_at = now()
			WHERE id = $1 AND tenant_id = $2`, order.ID, tenantID, ev.ProviderTradeNo); err != nil {
			return nil, fmt.Errorf("mark order paid: %w", err)
		}
		settledOrder := *order
		settledOrder.Status = OrderPaid
		if ev.ProviderTradeNo != "" && settledOrder.ProviderOrderID == "" {
			settledOrder.ProviderOrderID = ev.ProviderTradeNo
		}
		if err := s.notifyOrderSettled(ctx, tx, tenantID, &settledOrder); err != nil {
			return nil, err
		}
		if _, err := refreshProjectPaymentStatus(ctx, tx, tenantID, order.ProjectID); err != nil {
			return nil, err
		}
		detail := map[string]any{
			"project_id": order.ProjectID, "order_id": order.ID, "kind": order.Kind,
			"amount_cents": ev.AmountCents, "currency": ev.Currency, "event_id": ev.EventID,
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "payment.succeeded", "payment", order.ID.String(), "", detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "payment", order.ID.String(), "payment.succeeded", detail); err != nil {
			return nil, err
		}
		title, stage := "客户已支付定金", "shooting"
		if order.Kind == KindBalance {
			title, stage = "客户已支付尾款，可以开始选片", "selecting"
		} else if order.Kind == KindSelectionExtra {
			title, stage = "客户已支付补选精修费用", "retouch_delivery"
		} else if order.Kind == KindFull {
			title = "客户已完成付款"
		}
		if err := notice.EmitWorkflowEvent(ctx, tx, tenantID, order.ProjectID,
			"workflow:payment:"+order.ID.String(), title,
			fmt.Sprintf("已收到 %s %d 分。", order.Currency, ev.AmountCents),
			map[string]any{"project_id": order.ProjectID, "order_id": order.ID, "kind": order.Kind, "workflow_stage": stage, "event": "payment_succeeded"}); err != nil {
			return nil, err
		}
		return &CallbackResult{Status: "processed"}, nil
	})
}

func (s *Service) handleRefundCallback(ctx context.Context, providerName string, ev *CallbackEvent) (*CallbackResult, error) {
	var (
		tenantID uuid.UUID
		refundID uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
		SELECT tenant_id, refund_id FROM payment_refund_routes
		WHERE provider = $1 AND out_refund_no = $2`, providerName, ev.OutRefundNo).Scan(&tenantID, &refundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &CallbackResult{Status: "ignored"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup refund route: %w", err)
	}

	payload := ev.Raw
	if !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}

	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*CallbackResult, error) {
		refund, err := scanRefund(tx.QueryRow(ctx, `
			SELECT `+refundColumns+` FROM refunds WHERE id = $1 AND tenant_id = $2`, refundID, tenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return &CallbackResult{Status: "ignored"}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load refund: %w", err)
		}
		if _, err := lockProject(ctx, tx, tenantID, refund.ProjectID); err != nil {
			return nil, err
		}
		refund, err = scanRefund(tx.QueryRow(ctx, `
			SELECT `+refundColumns+` FROM refunds WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			refundID, tenantID))
		if err != nil {
			return nil, fmt.Errorf("lock refund: %w", err)
		}

		fresh, err := recordEvent(ctx, tx, tenantID, providerName, ev, payload, "applied")
		if err != nil {
			return nil, err
		}
		if !fresh {
			return &CallbackResult{Status: "duplicate"}, nil
		}
		if refund.Status != RefundPending {
			if err := setEventOutcome(ctx, tx, providerName, ev.EventID, "duplicate"); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "duplicate"}, nil
		}
		if ev.AmountCents != refund.AmountCents {
			if err := s.rejectEvent(ctx, tx, tenantID, providerName, ev, refund.ProjectID, "amount_mismatch", payload); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "rejected", Reason: "amount_mismatch"}, nil
		}
		if ev.Currency != refund.Currency {
			if err := s.rejectEvent(ctx, tx, tenantID, providerName, ev, refund.ProjectID, "currency_mismatch", payload); err != nil {
				return nil, err
			}
			return &CallbackResult{Status: "rejected", Reason: "currency_mismatch"}, nil
		}

		switch ev.Type {
		case EventRefundSucceeded:
			if _, err := tx.Exec(ctx, `
				UPDATE refunds SET status = 'succeeded',
					provider_refund_id = CASE WHEN $3 = '' THEN provider_refund_id ELSE $3 END,
					updated_at = now()
				WHERE id = $1 AND tenant_id = $2`, refund.ID, tenantID, ev.ProviderRefundID); err != nil {
				return nil, fmt.Errorf("mark refund succeeded: %w", err)
			}
			payment, err := scanPayment(tx.QueryRow(ctx, `
				SELECT `+paymentColumns+` FROM payments WHERE id = $1 AND tenant_id = $2`,
				refund.PaymentID, tenantID))
			if err != nil {
				return nil, fmt.Errorf("load refund payment: %w", err)
			}
			settledRefund := *refund
			settledRefund.Status = RefundSucceeded
			if ev.ProviderRefundID != "" && settledRefund.ProviderRefundID == "" {
				settledRefund.ProviderRefundID = ev.ProviderRefundID
			}
			if err := s.notifyRefundSettled(ctx, tx, tenantID, payment, &settledRefund); err != nil {
				return nil, err
			}
			if _, err := refreshProjectPaymentStatus(ctx, tx, tenantID, refund.ProjectID); err != nil {
				return nil, err
			}
			detail := map[string]any{
				"project_id": refund.ProjectID, "payment_id": refund.PaymentID,
				"amount_cents": ev.AmountCents, "event_id": ev.EventID,
			}
			if err := audit.Write(ctx, tx, tenantID, nil, "payment.refunded", "refund", refund.ID.String(), "", detail); err != nil {
				return nil, err
			}
			if err := outbox.Emit(ctx, tx, tenantID, "refund", refund.ID.String(), "payment.refunded", detail); err != nil {
				return nil, err
			}
		case EventRefundFailed:
			reason := strings.TrimSpace(ev.Reason)
			if reason == "" {
				reason = "refund failed at provider"
			}
			if _, err := tx.Exec(ctx, `
				UPDATE refunds SET status = 'failed', failure_reason = $3, updated_at = now()
				WHERE id = $1 AND tenant_id = $2`, refund.ID, tenantID, reason); err != nil {
				return nil, fmt.Errorf("mark refund failed: %w", err)
			}
			detail := map[string]any{
				"project_id": refund.ProjectID, "payment_id": refund.PaymentID,
				"reason": reason, "event_id": ev.EventID,
			}
			if err := audit.Write(ctx, tx, tenantID, nil, "payment.refund_failed", "refund", refund.ID.String(), "", detail); err != nil {
				return nil, err
			}
			if err := outbox.Emit(ctx, tx, tenantID, "refund", refund.ID.String(), "payment.refund_failed", detail); err != nil {
				return nil, err
			}
		}
		return &CallbackResult{Status: "processed"}, nil
	})
}

func (s *Service) settleRefund(ctx context.Context, tenantID, refundID uuid.UUID, providerRefundID string, raw json.RawMessage) (*Refund, error) {
	return db.WithTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*Refund, error) {
		refund, err := scanRefund(tx.QueryRow(ctx, `
			SELECT `+refundColumns+` FROM refunds WHERE id = $1 AND tenant_id = $2`, refundID, tenantID))
		if err != nil {
			return nil, fmt.Errorf("load refund: %w", err)
		}
		if _, err := lockProject(ctx, tx, tenantID, refund.ProjectID); err != nil {
			return nil, err
		}
		row := tx.QueryRow(ctx, `
			UPDATE refunds SET status = 'succeeded',
				provider_refund_id = CASE WHEN $3 = '' THEN provider_refund_id ELSE $3 END,
				updated_at = now()
			WHERE id = $1 AND tenant_id = $2
			RETURNING `+refundColumns, refundID, tenantID, providerRefundID)
		settled, err := scanRefund(row)
		if err != nil {
			return nil, fmt.Errorf("settle refund: %w", err)
		}
		payment, err := scanPayment(tx.QueryRow(ctx, `
			SELECT `+paymentColumns+` FROM payments WHERE id = $1 AND tenant_id = $2`,
			settled.PaymentID, tenantID))
		if err != nil {
			return nil, fmt.Errorf("load settled refund payment: %w", err)
		}
		if err := s.notifyRefundSettled(ctx, tx, tenantID, payment, settled); err != nil {
			return nil, err
		}
		if _, err := refreshProjectPaymentStatus(ctx, tx, tenantID, refund.ProjectID); err != nil {
			return nil, err
		}
		detail := map[string]any{
			"project_id": refund.ProjectID, "payment_id": refund.PaymentID,
			"amount_cents": refund.AmountCents,
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "payment.refunded", "refund", refundID.String(), "", detail); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "refund", refundID.String(), "payment.refunded", detail); err != nil {
			return nil, err
		}
		return settled, nil
	})
}

func (s *Service) markRefundFailed(ctx context.Context, tenantID, refundID uuid.UUID, reason string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		refund, err := scanRefund(tx.QueryRow(ctx, `
			SELECT `+refundColumns+` FROM refunds WHERE id = $1 AND tenant_id = $2`, refundID, tenantID))
		if err != nil {
			return err
		}
		if _, err := lockProject(ctx, tx, tenantID, refund.ProjectID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE refunds SET status = 'failed', failure_reason = $3, updated_at = now()
			WHERE id = $1 AND tenant_id = $2`, refundID, tenantID, reason); err != nil {
			return fmt.Errorf("mark refund failed: %w", err)
		}
		detail := map[string]any{"project_id": refund.ProjectID, "reason": reason}
		if err := audit.Write(ctx, tx, tenantID, nil, "payment.refund_failed", "refund", refundID.String(), "", detail); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, tenantID, "refund", refundID.String(), "payment.refund_failed", detail)
	})
}

func loadLedger(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID, lock bool) (*projectLedger, error) {
	query := `SELECT title,
		CASE WHEN state NOT IN ('cancelled','archived')
			AND EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status = 'cancelled')
			AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = projects.tenant_id AND b.project_id = projects.id AND b.status IN ('held','confirmed'))
			THEN 'cancelled' ELSE state END,
		currency, total_amount_cents, deposit_cents, change_amount_cents,
		payment_status FROM projects WHERE id = $1 AND tenant_id = $2`
	if lock {
		query += ` FOR UPDATE`
	}
	l := &projectLedger{}
	err := tx.QueryRow(ctx, query, projectID, tenantID).Scan(&l.Title, &l.State, &l.Currency, &l.TotalCents,
		&l.DepositCents, &l.ChangeCents, &l.PaymentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load project ledger: %w", err)
	}
	if l.State == "cancelled" || l.State == "archived" {
		return nil, ErrProjectClosed
	}
	// Project receivables exclude selection-extra charges, which are separate
	// service batches and must not affect deposit/balance completion.
	if err := tx.QueryRow(ctx, `
		SELECT
		  coalesce((SELECT sum(p.amount_cents) FROM payments p JOIN payment_orders o ON o.id=p.order_id
		            WHERE p.tenant_id = $1 AND p.project_id = $2 AND o.kind <> 'selection_extra'), 0)
		  - coalesce((SELECT sum(r.amount_cents) FROM refunds r JOIN payments p ON p.id=r.payment_id JOIN payment_orders o ON o.id=p.order_id
		              WHERE r.tenant_id = $1 AND r.project_id = $2 AND r.status = 'succeeded' AND o.kind <> 'selection_extra'), 0),
		  coalesce((SELECT sum(p.amount_cents) FROM payments p
		            JOIN payment_orders o ON o.id = p.order_id
			            WHERE p.tenant_id = $1 AND p.project_id = $2 AND o.kind IN ('deposit','balance','full')), 0)
		  - coalesce((SELECT sum(r.amount_cents) FROM refunds r
		              JOIN payments p ON p.id = r.payment_id
		              JOIN payment_orders o ON o.id = p.order_id
			              WHERE r.tenant_id = $1 AND r.project_id = $2 AND r.status = 'succeeded' AND o.kind IN ('deposit','balance','full')), 0)`,
		tenantID, projectID).Scan(&l.NetCents, &l.CoreNetCents); err != nil {
		return nil, fmt.Errorf("load project net paid: %w", err)
	}
	return l, nil
}

// orderAmount prices the contract receivables only. Change money is never part
// of the deposit/balance/full ladder; it is collected through its own order.
func orderAmount(kind string, l *projectLedger) (int64, error) {
	if l.TotalCents <= 0 {
		return 0, fmt.Errorf("%w: project has no total amount", ErrInvalidInput)
	}
	switch kind {
	case KindDeposit:
		if l.DepositCents <= 0 {
			return 0, fmt.Errorf("%w: project has no deposit configured", ErrInvalidInput)
		}
		if l.CoreNetCents >= l.DepositCents {
			return 0, fmt.Errorf("%w: deposit is already paid", ErrAlreadyPaid)
		}
		return l.DepositCents, nil
	case KindBalance:
		if l.DepositCents <= 0 {
			return 0, fmt.Errorf("%w: project has no deposit; use a full payment", ErrInvalidInput)
		}
		if l.CoreNetCents < l.DepositCents {
			return 0, ErrDepositUnpaid
		}
		if l.CoreNetCents >= l.TotalCents {
			return 0, fmt.Errorf("%w: project is fully paid", ErrAlreadyPaid)
		}
		balance := l.TotalCents - l.DepositCents
		if balance <= 0 {
			return 0, fmt.Errorf("%w: no balance due", ErrInvalidInput)
		}
		return balance, nil
	case KindFull:
		if l.DepositCents > 0 {
			return 0, fmt.Errorf("%w: project has a deposit; use deposit and balance orders", ErrInvalidInput)
		}
		if l.CoreNetCents >= l.TotalCents {
			return 0, fmt.Errorf("%w: project is fully paid", ErrAlreadyPaid)
		}
		return l.TotalCents, nil
	case KindChange:
		return 0, fmt.Errorf("%w: change orders are collected through the change order endpoint", ErrInvalidInput)
	default:
		return 0, fmt.Errorf("%w: unknown kind %q", ErrInvalidInput, kind)
	}
}

func orderAmountFor(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, in CreateOrderInput, l *projectLedger, lock bool) (int64, error) {
	if in.Kind == KindSelectionExtra {
		var amount int64
		query := `SELECT amount_cents FROM selection_charge_batches WHERE tenant_id=$1 AND project_id=$2 AND id=$3 AND status='pending'`
		if lock {
			query += ` FOR UPDATE`
		}
		if err := tx.QueryRow(ctx, query, tenantID, in.ProjectID, in.SelectionChargeBatchID).Scan(&amount); err != nil {
			return 0, fmt.Errorf("load selection charge batch: %w", err)
		}
		return amount, nil
	}
	if in.Kind == KindBalance {
		var published bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM galleries WHERE tenant_id=$1 AND project_id=$2 AND status='published' AND revoked_at IS NULL)`, tenantID, in.ProjectID).Scan(&published); err != nil {
			return 0, err
		}
		if !published {
			return 0, fmt.Errorf("%w: balance is collected after original gallery publication", ErrInvalidInput)
		}
	}
	if in.Kind != KindChange {
		return orderAmount(in.Kind, l)
	}
	co, err := loadChangeOrderLedger(ctx, tx, tenantID, in.ProjectID, *in.ChangeOrderID, lock)
	if err != nil {
		return 0, err
	}
	if co.Currency != l.Currency {
		return 0, fmt.Errorf("%w: change order currency %s does not match the project currency %s",
			ErrInvalidInput, co.Currency, l.Currency)
	}
	return co.AmountCents, nil
}

type changeOrderLedger struct {
	Status      string
	AmountCents int64
	Currency    string
}

// loadChangeOrderLedger guards the supplement order: only a signed change
// order that still awaits its payment can be collected.
func loadChangeOrderLedger(ctx context.Context, tx pgx.Tx, tenantID, projectID, changeOrderID uuid.UUID, lock bool) (*changeOrderLedger, error) {
	query := `SELECT status, amount_cents, currency FROM change_orders
		WHERE id = $1 AND tenant_id = $2 AND project_id = $3`
	if lock {
		query += ` FOR UPDATE`
	}
	var co changeOrderLedger
	err := tx.QueryRow(ctx, query, changeOrderID, tenantID, projectID).
		Scan(&co.Status, &co.AmountCents, &co.Currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: change order not found", ErrInvalidInput)
	}
	if err != nil {
		return nil, fmt.Errorf("load change order: %w", err)
	}
	if co.Status != "signed" && co.Status != "awaiting_payment" {
		return nil, fmt.Errorf("%w: change order is %s", ErrInvalidInput, co.Status)
	}
	return &co, nil
}

func liveOrder(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID, kind string, changeOrderID, selectionChargeBatchID *uuid.UUID, lock bool) (*Order, error) {
	query := `SELECT ` + orderColumns + ` FROM payment_orders
		WHERE tenant_id = $1 AND project_id = $2 AND kind = $3
		AND change_order_id IS NOT DISTINCT FROM $4::uuid
		AND selection_charge_batch_id IS NOT DISTINCT FROM $5::uuid
		AND status IN ('creating','pending')`
	if lock {
		query += ` FOR UPDATE`
	}
	order, err := scanOrder(tx.QueryRow(ctx, query, tenantID, projectID, kind, changeOrderID, selectionChargeBatchID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load live payment order: %w", err)
	}
	return order, nil
}

func reusableOrder(o *Order, amount int64, currency string) bool {
	return o.Status == OrderPending && o.AmountCents == amount &&
		o.Currency == currency && o.PayURL != "" && o.ExpiresAt.After(time.Now())
}

// lockOrder locks the project first and then the order, which is the global
// lock order for every payment path.
func lockOrder(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID) (*Order, error) {
	var projectID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT project_id FROM payment_orders WHERE id = $1 AND tenant_id = $2`,
		orderID, tenantID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("locate payment order: %w", err)
	}
	if _, err := lockProject(ctx, tx, tenantID, projectID); err != nil {
		return nil, err
	}
	order, err := scanOrder(tx.QueryRow(ctx, `
		SELECT `+orderColumns+` FROM payment_orders WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		orderID, tenantID))
	if err != nil {
		return nil, fmt.Errorf("lock payment order: %w", err)
	}
	return order, nil
}

func lockPayment(ctx context.Context, tx pgx.Tx, tenantID, paymentID uuid.UUID) (*Payment, error) {
	var projectID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT project_id FROM payments WHERE id = $1 AND tenant_id = $2`,
		paymentID, tenantID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("locate payment: %w", err)
	}
	if _, err := lockProject(ctx, tx, tenantID, projectID); err != nil {
		return nil, err
	}
	p, err := scanPayment(tx.QueryRow(ctx, `
		SELECT `+paymentColumns+` FROM payments WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		paymentID, tenantID))
	if err != nil {
		return nil, fmt.Errorf("lock payment: %w", err)
	}
	return p, nil
}

func lockProject(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `
		SELECT payment_status FROM projects WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		projectID, tenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrProjectNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lock project: %w", err)
	}
	return status, nil
}

func refreshProjectPaymentStatus(ctx context.Context, tx pgx.Tx, tenantID, projectID uuid.UUID) (string, error) {
	var total, deposit, change int64
	if err := tx.QueryRow(ctx, `
		SELECT total_amount_cents, deposit_cents, change_amount_cents
		FROM projects WHERE id = $1 AND tenant_id = $2`,
		projectID, tenantID).Scan(&total, &deposit, &change); err != nil {
		return "", fmt.Errorf("load project amounts: %w", err)
	}
	var net, coreNet int64
	if err := tx.QueryRow(ctx, `
		SELECT
		  coalesce((SELECT sum(p.amount_cents) FROM payments p JOIN payment_orders o ON o.id=p.order_id
		            WHERE p.tenant_id = $1 AND p.project_id = $2 AND o.kind <> 'selection_extra'), 0)
		  - coalesce((SELECT sum(r.amount_cents) FROM refunds r JOIN payments p ON p.id=r.payment_id JOIN payment_orders o ON o.id=p.order_id
		              WHERE r.tenant_id = $1 AND r.project_id = $2 AND r.status = 'succeeded' AND o.kind <> 'selection_extra'), 0),
		  coalesce((SELECT sum(p.amount_cents) FROM payments p
		            JOIN payment_orders o ON o.id = p.order_id
			            WHERE p.tenant_id = $1 AND p.project_id = $2 AND o.kind IN ('deposit','balance','full')), 0)
		  - coalesce((SELECT sum(r.amount_cents) FROM refunds r
		              JOIN payments p ON p.id = r.payment_id
		              JOIN payment_orders o ON o.id = p.order_id
			              WHERE r.tenant_id = $1 AND r.project_id = $2 AND r.status = 'succeeded' AND o.kind IN ('deposit','balance','full')), 0)`,
		tenantID, projectID).Scan(&net, &coreNet); err != nil {
		return "", fmt.Errorf("sum project payments: %w", err)
	}
	var pendingDeposit, pendingBalance, pendingFull bool
	rows, err := tx.Query(ctx, `
		SELECT kind FROM payment_orders
		WHERE tenant_id = $1 AND project_id = $2 AND status = 'pending' AND expires_at > now()`,
		tenantID, projectID)
	if err != nil {
		return "", fmt.Errorf("load pending payment orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return "", fmt.Errorf("scan pending order kind: %w", err)
		}
		switch kind {
		case KindDeposit:
			pendingDeposit = true
		case KindBalance:
			pendingBalance = true
		case KindFull:
			pendingFull = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	status := DeriveProjectStatus(total, total+change, deposit, net, coreNet,
		pendingDeposit, pendingFull, pendingBalance)
	if _, err := tx.Exec(ctx, `
		UPDATE projects SET payment_status = $3, updated_at = now()
		WHERE id = $1 AND tenant_id = $2`, projectID, tenantID, status); err != nil {
		return "", fmt.Errorf("sync project payment status: %w", err)
	}
	return status, nil
}

func refundableAmount(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p *Payment) (int64, error) {
	var committed int64
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(sum(amount_cents), 0) FROM refunds
		WHERE tenant_id = $1 AND payment_id = $2 AND status IN ('pending','succeeded')`,
		tenantID, p.ID).Scan(&committed); err != nil {
		return 0, fmt.Errorf("sum refunds: %w", err)
	}
	remaining := p.AmountCents - committed
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

func recordEvent(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, provider string, ev *CallbackEvent, payload json.RawMessage, outcome string) (bool, error) {
	refNo := ev.OutTradeNo
	if ev.OutRefundNo != "" {
		refNo = ev.OutRefundNo
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO payment_events (tenant_id, provider, event_id, event_type, ref_no, outcome, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (provider, event_id) DO NOTHING`,
		tenantID, provider, ev.EventID, ev.Type, refNo, outcome, payload)
	if err != nil {
		return false, fmt.Errorf("record payment event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func setEventOutcome(ctx context.Context, tx pgx.Tx, provider, eventID, outcome string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE payment_events SET outcome = $3 WHERE provider = $1 AND event_id = $2`,
		provider, eventID, outcome); err != nil {
		return fmt.Errorf("update payment event outcome: %w", err)
	}
	return nil
}

// rejectEvent writes the security trail for an authentic callback that must not
// be credited: the event is kept with outcome "rejected", audited and surfaced
// as a critical alert.
func (s *Service) rejectEvent(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, provider string, ev *CallbackEvent, projectID uuid.UUID, reason string, payload json.RawMessage) error {
	if err := setEventOutcome(ctx, tx, provider, ev.EventID, "rejected"); err != nil {
		return err
	}
	detail := map[string]any{
		"project_id": projectID, "reason": reason, "event_id": ev.EventID,
		"event_type": ev.Type, "out_trade_no": ev.OutTradeNo, "out_refund_no": ev.OutRefundNo,
		"amount_cents": ev.AmountCents, "currency": ev.Currency,
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "payment.callback_rejected", "payment_event", ev.EventID, "", detail); err != nil {
		return err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "payment_event", ev.EventID, "payment.callback_rejected", detail); err != nil {
		return err
	}
	if s.alerts != nil {
		code := "callback_" + reason
		amount := ev.AmountCents
		if _, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
			ProjectID:   &projectID,
			SubjectType: "payment_event",
			SubjectID:   ev.EventID,
			Code:        code,
			Severity:    alert.SeverityCritical,
			DedupeKey:   code + ":" + ev.EventID,
			Title:       "Authentic payment callback rejected: " + reason,
			Detail:      detail,
			AmountCents: &amount,
		}); err != nil {
			s.log.Error("raise payment alert", "code", code, "event_id", ev.EventID, "err", err)
		}
	}
	return nil
}

// flagLatePayment records an authentic payment for an order that is no longer
// payable. Funds were captured at the channel, so this must surface as an
// alert (refund or manual credit) instead of being credited or dropped.
func (s *Service) flagLatePayment(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, provider string, ev *CallbackEvent, order *Order) error {
	if err := setEventOutcome(ctx, tx, provider, ev.EventID, "flagged"); err != nil {
		return err
	}
	detail := map[string]any{
		"project_id": order.ProjectID, "order_id": order.ID, "kind": order.Kind,
		"order_status": order.Status, "out_trade_no": ev.OutTradeNo,
		"provider_trade_no": ev.ProviderTradeNo, "event_id": ev.EventID,
		"amount_cents": ev.AmountCents, "currency": ev.Currency,
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "payment.late_payment", "payment_order", order.ID.String(), "", detail); err != nil {
		return err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "payment_order", order.ID.String(), "payment.late_payment", detail); err != nil {
		return err
	}
	if s.alerts != nil {
		amount := ev.AmountCents
		if _, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
			ProjectID:   &order.ProjectID,
			SubjectType: "payment_order",
			SubjectID:   order.ID.String(),
			Code:        "late_payment",
			Severity:    alert.SeverityCritical,
			DedupeKey:   "late_payment:" + order.ID.String() + ":" + ev.EventID,
			Title:       "Payment captured for an order that is no longer payable",
			Detail:      detail,
			AmountCents: &amount,
		}); err != nil {
			s.log.Error("raise late payment alert", "order_id", order.ID, "event_id", ev.EventID, "err", err)
		}
	}
	return nil
}

// raiseStandalone opens its own tenant transaction for alerts raised outside an
// existing business transaction. Failures are logged, never propagated.
func (s *Service) raiseStandalone(ctx context.Context, tenantID uuid.UUID, in alert.Input) {
	if s.alerts == nil {
		return
	}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := s.alerts.Raise(ctx, tx, tenantID, in)
		return err
	})
	if err != nil {
		s.log.Error("raise payment alert", "code", in.Code, "dedupe_key", in.DedupeKey, "err", err)
	}
}

// HandleMerchantMismatch records a callback whose signature was valid but that
// was issued for a different merchant. The route lookup decides whether the
// finding can be attached to a tenant; metrics and the log always record it.
func (s *Service) HandleMerchantMismatch(ctx context.Context, providerName string, body []byte) {
	httpx.Inc("payment_security_events_total", map[string]string{"code": "merchant_mismatch"})
	var probe struct {
		EventID     string `json:"event_id"`
		Type        string `json:"type"`
		OutTradeNo  string `json:"out_trade_no"`
		OutRefundNo string `json:"out_refund_no"`
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
	}
	_ = json.Unmarshal(body, &probe)
	s.log.Error("payment callback from foreign merchant", "provider", providerName,
		"event_id", probe.EventID, "out_trade_no", probe.OutTradeNo)

	if s.alerts == nil || probe.OutTradeNo == "" {
		return
	}
	var tenantID uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		SELECT tenant_id FROM payment_order_routes WHERE provider = $1 AND out_trade_no = $2`,
		providerName, probe.OutTradeNo).Scan(&tenantID); err != nil {
		return
	}
	dedupe := "merchant_mismatch:" + probe.EventID
	if probe.EventID == "" {
		dedupe = "merchant_mismatch:" + probe.OutTradeNo
	}
	in := alert.Input{
		SubjectType: "payment_route",
		SubjectID:   probe.OutTradeNo,
		Code:        "merchant_mismatch",
		Severity:    alert.SeverityCritical,
		DedupeKey:   dedupe,
		Title:       "Payment callback was signed for another merchant",
		Detail: map[string]any{
			"provider": providerName, "event_id": probe.EventID, "event_type": probe.Type,
			"out_trade_no": probe.OutTradeNo, "out_refund_no": probe.OutRefundNo,
			"amount_cents": probe.AmountCents, "currency": probe.Currency,
		},
	}
	if probe.AmountCents > 0 {
		amount := probe.AmountCents
		in.AmountCents = &amount
	}
	s.raiseStandalone(ctx, tenantID, in)
}

func newTradeNo(prefix string) (string, error) {
	raw, err := randomHex(10)
	if err != nil {
		return "", err
	}
	return prefix + strings.ToUpper(raw), nil
}

// callerActor maps the zero uuid (system/callback callers) to nil so nullable
// actor columns and their foreign keys accept automated settlement paths.
func callerActor(actor uuid.UUID) *uuid.UUID {
	if actor == uuid.Nil {
		return nil
	}
	return &actor
}

func scanOrder(row rowScanner) (*Order, error) {
	var (
		o                      Order
		contractID             *uuid.UUID
		changeOrderID          *uuid.UUID
		selectionChargeBatchID *uuid.UUID
	)
	if err := row.Scan(&o.ID, &o.ProjectID, &contractID, &o.Kind, &changeOrderID, &selectionChargeBatchID, &o.Provider, &o.Channel,
		&o.Currency, &o.AmountCents, &o.Status, &o.OutTradeNo, &o.ProviderOrderID,
		&o.PayURL, &o.ExpiresAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, err
	}
	o.ContractID = contractID
	o.ChangeOrderID = changeOrderID
	o.SelectionChargeBatchID = selectionChargeBatchID
	return &o, nil
}

func scanPayment(row rowScanner) (*Payment, error) {
	var p Payment
	if err := row.Scan(&p.ID, &p.ProjectID, &p.OrderID, &p.Provider, &p.Channel,
		&p.OutTradeNo, &p.ProviderTradeNo, &p.AmountCents, &p.Currency, &p.PaidAt,
		&p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func scanRefund(row rowScanner) (*Refund, error) {
	var r Refund
	if err := row.Scan(&r.ID, &r.ProjectID, &r.PaymentID, &r.Provider, &r.OutRefundNo,
		&r.ProviderRefundID, &r.AmountCents, &r.Currency, &r.Reason, &r.Status,
		&r.FailureReason, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}
