package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Emit writes an event in the caller's transaction. The dispatcher publishes it
// afterwards; business writes never depend on the publisher being up.
func Emit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, aggregateType, aggregateID, eventType string, payload any) error {
	raw := json.RawMessage(`{}`)
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal outbox payload: %w", err)
		}
		raw = b
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (tenant_id, aggregate_type, aggregate_id, event_type, payload)
		VALUES ($1,$2,$3,$4,$5)`,
		tenantID, aggregateType, aggregateID, eventType, raw); err != nil {
		return fmt.Errorf("emit outbox event: %w", err)
	}
	return nil
}
