package payment

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type bothHooks struct{}

func (bothHooks) OrderSettled(context.Context, pgx.Tx, uuid.UUID, *Order) error { return nil }

func (bothHooks) RefundSettled(context.Context, pgx.Tx, uuid.UUID, *Payment, *Refund) error {
	return nil
}

type refundOnlyHook struct{}

func (refundOnlyHook) RefundSettled(context.Context, pgx.Tx, uuid.UUID, *Payment, *Refund) error {
	return nil
}

func TestSetHooksRegistersEveryCapability(t *testing.T) {
	var s Service
	s.SetHooks(bothHooks{}, refundOnlyHook{})
	if len(s.orderHooks) != 1 {
		t.Fatalf("order hooks = %d, want 1", len(s.orderHooks))
	}
	if len(s.refundHooks) != 2 {
		t.Fatalf("refund hooks = %d, want 2 (a hook implementing both must be registered twice)", len(s.refundHooks))
	}
}
