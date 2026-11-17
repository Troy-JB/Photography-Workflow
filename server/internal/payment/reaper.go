package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/outbox"
)

// SweepCreatingOrders closes reservations that never reached the channel.
// A request can crash after the local "creating" row is committed and before
// the provider call, or before the pay URL is published; without a sweep those
// rows would linger forever unless the customer retried the same order.
//
// It runs on the worker role, which is allowed to scan all tenants. Orders are
// closed one by one: the channel is asked first, then the local row is closed
// under a status + age guard so a concurrent retry that already published a
// pay URL is never clobbered. A reservation the channel actually captured is
// alerted and left in place for reconciliation instead of being closed.
func SweepCreatingOrders(ctx context.Context, pool *pgxpool.Pool, provider Provider,
	alerts *alert.Service, grace time.Duration, limit int, log *slog.Logger) (int, error) {
	if provider == nil {
		return 0, nil
	}
	if grace <= 0 {
		grace = OrderCreateGrace
	}
	if limit <= 0 {
		limit = 50
	}

	type candidate struct {
		ID         uuid.UUID
		TenantID   uuid.UUID
		ProjectID  uuid.UUID
		Kind       string
		OutTradeNo string
		Amount     int64
		Currency   string
	}
	rows, err := pool.Query(ctx, `
		SELECT id, tenant_id, project_id, kind, out_trade_no, amount_cents, currency
		FROM payment_orders
		WHERE status = 'creating' AND created_at < now() - make_interval(secs => $1)
		ORDER BY created_at
		LIMIT $2`, grace.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("scan creating payment orders: %w", err)
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ProjectID, &c.Kind, &c.OutTradeNo,
			&c.Amount, &c.Currency); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan creating payment order: %w", err)
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scan creating payment orders: %w", err)
	}

	closed := 0
	for _, c := range candidates {
		state, qerr := provider.QueryOrder(ctx, c.OutTradeNo)
		switch {
		case errors.Is(qerr, ErrProviderOrderNotFound):
			// The channel never saw the order; closing locally is safe.
			n, err := closeSweptOrder(ctx, pool, c.ID, c.TenantID, c.ProjectID, c.Kind,
				c.OutTradeNo, grace, "provider_not_found")
			if err != nil {
				log.Error("close swept payment order", "order_id", c.ID, "err", err)
				continue
			}
			closed += n
		case qerr != nil:
			log.Warn("query creating payment order", "order_id", c.ID, "err", qerr)
		case state.Status == OrderPaid:
			raiseCreatingPaid(ctx, pool, alerts, log, c.ID, c.TenantID, c.ProjectID, c.Kind,
				c.OutTradeNo, c.Amount, c.Currency)
		case state.Status == OrderPending:
			// A channel order exists but the pay URL was never published, so the
			// customer cannot pay it. Close at the channel first; a capture that
			// raced with the close surfaces as ErrProviderOrderPaid and must be
			// reconciled, never hidden behind a local close.
			switch cerr := provider.CloseOrder(ctx, c.OutTradeNo); {
			case errors.Is(cerr, ErrProviderOrderPaid):
				raiseCreatingPaid(ctx, pool, alerts, log, c.ID, c.TenantID, c.ProjectID, c.Kind,
					c.OutTradeNo, c.Amount, c.Currency)
				continue
			case cerr != nil:
				log.Warn("close unpublished channel order", "order_id", c.ID, "err", cerr)
				continue
			}
			n, err := closeSweptOrder(ctx, pool, c.ID, c.TenantID, c.ProjectID, c.Kind,
				c.OutTradeNo, grace, "unpublished")
			if err != nil {
				log.Error("close swept payment order", "order_id", c.ID, "err", err)
				continue
			}
			closed += n
		case state.Status == OrderClosed:
			n, err := closeSweptOrder(ctx, pool, c.ID, c.TenantID, c.ProjectID, c.Kind,
				c.OutTradeNo, grace, "provider_closed")
			if err != nil {
				log.Error("close swept payment order", "order_id", c.ID, "err", err)
				continue
			}
			closed += n
		default:
			log.Warn("creating payment order has unknown channel state",
				"order_id", c.ID, "status", state.Status)
		}
	}
	return closed, nil
}

func closeSweptOrder(ctx context.Context, pool *pgxpool.Pool, orderID, tenantID, projectID uuid.UUID,
	kind, outTradeNo string, grace time.Duration, reason string) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin sweep order close: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The guard mirrors the scan: a retry that turned the reservation into a
	// pending order (or replaced the row) must not be closed here.
	tag, err := tx.Exec(ctx, `
		UPDATE payment_orders SET status = 'closed', updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND status = 'creating'
			AND created_at < now() - make_interval(secs => $3)`,
		orderID, tenantID, grace.Seconds())
	if err != nil {
		return 0, fmt.Errorf("close swept payment order: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, nil
	}
	detail := map[string]any{
		"project_id": projectID, "order_id": orderID, "kind": kind,
		"out_trade_no": outTradeNo, "reason": "sweep_" + reason,
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "payment.order_closed", "payment_order",
		orderID.String(), "", detail); err != nil {
		return 0, err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "payment_order", orderID.String(),
		"payment.order_closed", detail); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit swept payment order close: %w", err)
	}
	return 1, nil
}

// raiseCreatingPaid surfaces a captured payment for a reservation whose order
// was never published. The row stays "creating" so a reconcile can credit it.
func raiseCreatingPaid(ctx context.Context, pool *pgxpool.Pool, alerts *alert.Service,
	log *slog.Logger, orderID, tenantID, projectID uuid.UUID, kind, outTradeNo string,
	amount int64, currency string) {
	if alerts == nil {
		log.Error("creating order was paid at the channel; no alert sink configured",
			"order_id", orderID, "out_trade_no", outTradeNo)
		return
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Error("begin creating-paid alert", "order_id", orderID, "err", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	project := projectID
	if _, err := alerts.Raise(ctx, tx, tenantID, alert.Input{
		ProjectID:   &project,
		SubjectType: "payment_order",
		SubjectID:   orderID.String(),
		Code:        "creating_order_paid",
		Severity:    alert.SeverityCritical,
		DedupeKey:   "sweep_paid:" + orderID.String(),
		Title:       "A payment was captured for a payment order that was never published",
		Detail: map[string]any{
			"order_id": orderID, "kind": kind, "out_trade_no": outTradeNo,
			"amount_cents": amount, "currency": currency,
		},
		AmountCents: &amount,
	}); err != nil {
		log.Error("raise creating-paid alert", "order_id", orderID, "err", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Error("commit creating-paid alert", "order_id", orderID, "err", err)
	}
}
