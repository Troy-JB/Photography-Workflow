package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/proof"
)

// SMSProvider is the outbound SMS contract. Only the sandbox implementation
// ships today; a real carrier adapter plugs in behind the same interface and
// must be added before production enables SMS.
type SMSProvider interface {
	Name() string
	Send(ctx context.Context, to, text string) error
}

const SMSSandbox = "sandbox"

// NewSMSProvider builds the configured provider. An empty provider disables
// SMS; CheckProduction already refuses sandbox outside development.
func NewSMSProvider(name string, log *slog.Logger) (SMSProvider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return nil, nil
	case SMSSandbox:
		if log == nil {
			log = slog.Default()
		}
		return sandboxSMS{log: log}, nil
	default:
		return nil, fmt.Errorf("sms provider %q is not supported (empty or sandbox)", name)
	}
}

// sandboxSMS records the attempt and drops the message. It never logs the
// proof link or the full phone number.
type sandboxSMS struct {
	log *slog.Logger
}

func (s sandboxSMS) Name() string { return SMSSandbox }

func (s sandboxSMS) Send(_ context.Context, to, text string) error {
	s.log.Info("sms sandbox: message not sent", "to", maskPhone(to), "chars", len(text))
	return nil
}

func maskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if len(phone) <= 4 {
		return "****"
	}
	return "****" + phone[len(phone)-4:]
}

// SMSNotifier adapts the provider to proof.SMSSender: it resolves the client
// phone inside the caller's transaction and leaves an audit trail under the
// sandbox provider, where a human still has to pass the link on.
type SMSNotifier struct {
	provider      SMSProvider
	publicBaseURL string
}

func NewSMSNotifier(provider SMSProvider, publicBaseURL string) *SMSNotifier {
	if provider == nil {
		return nil
	}
	return &SMSNotifier{provider: provider, publicBaseURL: strings.TrimRight(publicBaseURL, "/")}
}

func (n *SMSNotifier) SendProofLink(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, round *proof.Round,
	plaintext string, expiresAt *time.Time) error {
	var phone string
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(c.phone, '') FROM projects p
		LEFT JOIN clients c ON c.tenant_id = p.tenant_id AND c.id = p.client_id
		WHERE p.tenant_id = $1 AND p.id = $2`, tenantID, round.ProjectID).Scan(&phone); err != nil {
		return fmt.Errorf("load client phone: %w", err)
	}
	if strings.TrimSpace(phone) == "" {
		return proof.ErrClientPhoneMissing
	}
	text := fmt.Sprintf("第 %d 轮审片已开放：%s/#/p/%s", round.RoundNo, n.publicBaseURL, plaintext)
	if expiresAt != nil {
		text += "（" + expiresAt.Format("2006-01-02 15:04") + " 前有效）"
	}
	if err := n.provider.Send(ctx, phone, text); err != nil {
		return fmt.Errorf("send proof sms: %w", err)
	}
	detail := map[string]any{"round_no": round.RoundNo, "provider": n.provider.Name()}
	if expiresAt != nil {
		detail["expires_at"] = expiresAt
	}
	return audit.Write(ctx, tx, tenantID, nil, "proof.sms.queued", "proof_round", round.ID.String(), "", detail)
}
