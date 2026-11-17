package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/delivery"
)

// DeliveryNotifierConfig carries the delivery template switches. Email stays
// optional: without SMTP the studio can still copy the one-time link.
type DeliveryNotifierConfig struct {
	EmailEnabled bool
	Timezone     *time.Location
}

// DeliveryNotifier implements delivery.Notifier. It runs inside the caller's
// tenant transaction, so a rolled-back token never leaves mail queued.
type DeliveryNotifier struct {
	cfg DeliveryNotifierConfig
	log *slog.Logger
}

func NewDeliveryNotifier(cfg DeliveryNotifierConfig, log *slog.Logger) *DeliveryNotifier {
	if cfg.Timezone == nil {
		cfg.Timezone = time.UTC
	}
	return &DeliveryNotifier{cfg: cfg, log: log}
}

func (n *DeliveryNotifier) EmailAvailable() bool { return n.cfg.EmailEnabled }

// DeliveryReady queues the customer mail with the one-time download link. The
// link only exists in the message body until the worker sends it; the body is
// cleared afterwards by notify.Service.
func (n *DeliveryNotifier) DeliveryReady(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	p delivery.NotifyPayload) (uuid.UUID, error) {
	subject, body := deliveryReadyMail(p, n.cfg.Timezone)
	out := Outgoing{
		TenantID:    tenantID,
		ToEmail:     strings.TrimSpace(p.ClientEmail),
		Subject:     subject,
		Body:        body,
		DedupeKey:   fmt.Sprintf("delivery_ready:%s:%s", p.DeliveryID, p.TokenID),
		RelatedType: "delivery",
		RelatedID:   &p.DeliveryID,
		Payload: map[string]any{
			"delivery_id": p.DeliveryID,
			"project_id":  p.ProjectID,
			"token_id":    p.TokenID,
		},
	}
	switch {
	case !n.cfg.EmailEnabled:
		out.Status = StatusSkipped
		out.LastError = "email channel is not configured"
	case out.ToEmail == "":
		out.Status = StatusSkipped
		out.LastError = "project client has no email"
	}
	id, err := Enqueue(ctx, tx, out)
	if err != nil {
		return uuid.Nil, err
	}
	if out.Status == StatusSkipped && n.log != nil {
		n.log.Info("delivery email skipped", "delivery_id", p.DeliveryID, "reason", out.LastError)
	}
	return id, nil
}

// deliveryReadyMail is pure so the template can be asserted without a DB.
func deliveryReadyMail(p delivery.NotifyPayload, tz *time.Location) (string, string) {
	title := strings.TrimSpace(p.ProjectTitle)
	if title == "" {
		title = "您的照片"
	}
	subject := fmt.Sprintf("《%s》交付文件已就绪", title)
	var b strings.Builder
	b.WriteString("您好：\n\n")
	fmt.Fprintf(&b, "《%s》的成品已整理完成，共 %d 张。\n\n", title, p.ItemCount)
	b.WriteString("下载包：\n")
	for _, pkg := range p.Packages {
		if pkg.Status != delivery.PackageReady {
			continue
		}
		label := "网页版（长边 2048，JPEG）"
		if pkg.Spec == delivery.SpecOriginal {
			label = "原图（与拍摄文件一致）"
		}
		fmt.Fprintf(&b, "- %s：%s，%d 个文件\n", label, humanBytes(pkg.SizeBytes), pkg.EntryCount)
	}
	fmt.Fprintf(&b, "\n下载链接：\n\n%s\n\n", p.URL)
	fmt.Fprintf(&b, "有效期至 %s，可下载 %d 次。链接仅供您本人使用，请勿转发。\n",
		p.ExpiresAt.In(tz).Format("2006-01-02 15:04"), p.MaxDownloads)
	b.WriteString("\n如链接失效或需要更多下载次数，请联系工作室重新发送。\n")
	return subject, b.String()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
