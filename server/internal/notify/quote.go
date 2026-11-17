package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/quote"
)

type QuoteNotifierConfig struct {
	EmailEnabled bool
	ClientURL    string
}

type QuoteNotifier struct {
	cfg QuoteNotifierConfig
	log *slog.Logger
}

func NewQuoteNotifier(cfg QuoteNotifierConfig, log *slog.Logger) *QuoteNotifier {
	cfg.ClientURL = strings.TrimRight(strings.TrimSpace(cfg.ClientURL), "/")
	return &QuoteNotifier{cfg: cfg, log: log}
}

func (n *QuoteNotifier) QuoteSent(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, q *quote.Quote) error {
	var projectTitle, clientName, clientEmail string
	if err := tx.QueryRow(ctx, `
		SELECT p.title, coalesce(c.name, ''), coalesce(c.email, '')
		FROM projects p
		LEFT JOIN clients c ON c.id = p.client_id AND c.tenant_id = p.tenant_id
		WHERE p.tenant_id = $1 AND p.id = $2`, tenantID, q.ProjectID).
		Scan(&projectTitle, &clientName, &clientEmail); err != nil {
		return fmt.Errorf("load quote recipient: %w", err)
	}

	var body strings.Builder
	if clientName != "" {
		fmt.Fprintf(&body, "%s 您好：\n\n", clientName)
	}
	fmt.Fprintf(&body, "摄影师已为项目《%s》发送报价 v%d。\n\n", projectTitle, q.Version)
	fmt.Fprintf(&body, "报价总额：%s %d.%02d\n", q.Currency, q.TotalCents/100, q.TotalCents%100)
	if q.DepositCents > 0 {
		fmt.Fprintf(&body, "预约定金：%s %d.%02d\n", q.Currency, q.DepositCents/100, q.DepositCents%100)
	}
	if q.ValidUntil != nil {
		fmt.Fprintf(&body, "有效期至：%s\n", q.ValidUntil.Format("2006-01-02"))
	}
	if q.Terms != "" {
		fmt.Fprintf(&body, "\n报价条款：\n%s\n", q.Terms)
	}
	if n.cfg.ClientURL != "" {
		fmt.Fprintf(&body, "\n请打开客户中心查看报价：%s/#/pages/projects/projects\n", n.cfg.ClientURL)
	} else {
		body.WriteString("\n请打开微信中的摄影客户中心，在“我的拍摄”查看报价。\n")
	}

	out := Outgoing{
		TenantID:    tenantID,
		ToEmail:     strings.TrimSpace(clientEmail),
		Subject:     fmt.Sprintf("项目《%s》的新报价", projectTitle),
		Body:        body.String(),
		DedupeKey:   "quote_sent:" + q.ID.String(),
		RelatedType: "quote",
		RelatedID:   &q.ID,
		Payload: map[string]any{
			"project_id": q.ProjectID,
			"quote_id":   q.ID,
			"version":    q.Version,
		},
	}
	switch {
	case !n.cfg.EmailEnabled:
		out.Status = StatusSkipped
		out.LastError = "email channel is not configured"
	case strings.TrimSpace(clientEmail) == "":
		out.Status = StatusSkipped
		out.LastError = "project client has no email"
	default:
		address, err := mail.ParseAddress(out.ToEmail)
		if err != nil || address.Address != out.ToEmail {
			out.Status = StatusSkipped
			out.LastError = "project client email is invalid"
		}
	}
	if _, err := Enqueue(ctx, tx, out); err != nil {
		return fmt.Errorf("queue quote email: %w", err)
	}
	if out.Status == StatusSkipped && n.log != nil {
		n.log.Info("quote email skipped", "quote_id", q.ID, "reason", out.LastError)
	}
	return nil
}
