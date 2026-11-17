package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Write appends an audit record inside the caller's transaction. Payment,
// contract, refund, download, authorization and state changes must call this so
// the audit trail commits with the business write.
func Write(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actor *uuid.UUID, action, objectType, objectID, ip string, detail any) error {
	raw := json.RawMessage(`{}`)
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		raw = b
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (tenant_id, actor_user_id, action, object_type, object_id, ip, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		tenantID, actor, action, objectType, objectID, ip, raw); err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	return nil
}
