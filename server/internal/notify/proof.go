package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/proof"
)

// ProofNotifierConfig carries what proof templates need. Email stays optional
// so a deployment can run proof entirely through copied links.
type ProofNotifierConfig struct {
	PublicBaseURL string
	EmailEnabled  bool
	Timezone      *time.Location
}

// ProofNotifier implements proof.Notifier. Every method runs inside the
// caller's tenant transaction: mail is queued only when the surrounding state
// change commits, so a rolled-back round never leaves a live link in the queue.
type ProofNotifier struct {
	cfg ProofNotifierConfig
	log *slog.Logger
}

func NewProofNotifier(cfg ProofNotifierConfig, log *slog.Logger) *ProofNotifier {
	if cfg.Timezone == nil {
		cfg.Timezone = time.UTC
	}
	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	return &ProofNotifier{cfg: cfg, log: log}
}

func (n *ProofNotifier) EmailAvailable() bool { return n.cfg.EmailEnabled }

// proofMailData is the shared input of the three customer templates.
type proofMailData struct {
	ClientName   string
	ProjectTitle string
	GalleryTitle string
	RoundNo      int
	DueAt        time.Time
	URL          string
}

func (n *ProofNotifier) proofURL(token string) string {
	return n.cfg.PublicBaseURL + "/#/p/" + token
}

func (n *ProofNotifier) dueText(t time.Time) string {
	return t.In(n.cfg.Timezone).Format("2006-01-02 15:04")
}

// loadMailData resolves titles and the client name. The recipient itself comes
// from the round snapshot so a later client edit cannot silently retarget mail.
func loadMailData(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, round *proof.Round) (*proofMailData, error) {
	data := &proofMailData{RoundNo: round.RoundNo, DueAt: round.DueAt}
	if err := tx.QueryRow(ctx, `
		SELECT p.title, g.title, coalesce(c.name, '')
		FROM projects p
		JOIN galleries g ON g.tenant_id = p.tenant_id AND g.id = $3
		LEFT JOIN clients c ON c.tenant_id = p.tenant_id AND c.id = p.client_id
		WHERE p.tenant_id = $1 AND p.id = $2`,
		tenantID, round.ProjectID, round.GalleryID).
		Scan(&data.ProjectTitle, &data.GalleryTitle, &data.ClientName); err != nil {
		return nil, fmt.Errorf("load proof mail context: %w", err)
	}
	return data, nil
}

func (n *ProofNotifier) ProofInvite(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	round *proof.Round, token *proof.IssuedToken) error {
	data, err := loadMailData(ctx, tx, tenantID, round)
	if err != nil {
		return err
	}
	data.URL = n.proofURL(token.Plaintext)
	subject, body := inviteMail(data, n.dueText(data.DueAt))
	return n.enqueue(ctx, tx, tenantID, round, token, subject, body, "", "")
}

func (n *ProofNotifier) ProofResubmitted(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	round *proof.Round, token *proof.IssuedToken) error {
	data, err := loadMailData(ctx, tx, tenantID, round)
	if err != nil {
		return err
	}
	data.URL = n.proofURL(token.Plaintext)
	subject, body := resubmittedMail(data)
	return n.enqueue(ctx, tx, tenantID, round, token, subject, body, "", "")
}

// ProofDeadline queues the 24h reminder. The worker calls it without access to
// projects or clients, so it only uses the round snapshot and its recipient.
func (n *ProofNotifier) ProofDeadline(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	round *proof.Round, token *proof.IssuedToken) error {
	data := &proofMailData{
		RoundNo: round.RoundNo,
		DueAt:   round.DueAt,
		URL:     n.proofURL(token.Plaintext),
	}
	if galleryTitle, err := roundGalleryTitle(ctx, tx, tenantID, round.GalleryID); err == nil {
		data.GalleryTitle = galleryTitle
	}
	subject, body := deadlineMail(data, n.dueText(data.DueAt))
	return n.enqueue(ctx, tx, tenantID, round, token, subject, body,
		"proof_deadline:"+round.ID.String(), "proof deadline reminder")
}

// roundGalleryTitle tolerates a missing gallery title on the reminder path; the
// mail still makes sense without it.
func roundGalleryTitle(ctx context.Context, tx pgx.Tx, tenantID, galleryID uuid.UUID) (string, error) {
	var title string
	err := tx.QueryRow(ctx, `
		SELECT title FROM galleries WHERE tenant_id = $1 AND id = $2`,
		tenantID, galleryID).Scan(&title)
	return title, err
}

// enqueue writes the mail row or, when it cannot be delivered, a skipped row
// that explains why. Proof actions must never fail because a channel is off.
func (n *ProofNotifier) enqueue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	round *proof.Round, token *proof.IssuedToken, subject, body, dedupe, relatedNote string) error {
	out := Outgoing{
		TenantID:    tenantID,
		ToEmail:     strings.TrimSpace(round.ClientEmail),
		Subject:     subject,
		Body:        body,
		DedupeKey:   dedupe,
		RelatedType: "proof_round",
		RelatedID:   &round.ID,
		Payload: map[string]any{
			"proof_round_id": round.ID,
			"token_id":       token.ID,
			"project_id":     round.ProjectID,
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
	if _, err := Enqueue(ctx, tx, out); err != nil {
		return err
	}
	if out.Status == StatusSkipped && n.log != nil {
		n.log.Info("proof email skipped", "round_id", round.ID, "reason", out.LastError, "kind", relatedNote)
	}
	return nil
}

// ProofDecision records the customer's verdict for staff. The outbox event is
// emitted by the proof service; this row feeds the in-app list and webhook
// dispatcher.
func (n *ProofNotifier) ProofDecision(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID,
	round *proof.Round, decision string) error {
	kind := notice.TypeProofChanges
	title := "客户提出修改"
	body := fmt.Sprintf("第 %d 轮审片客户提出了修改意见，请查看批注。", round.RoundNo)
	if decision == proof.StatusApproved {
		kind = notice.TypeProofApproved
		title = "审片已通过"
		body = fmt.Sprintf("第 %d 轮审片客户已通过。", round.RoundNo)
	}
	payload, err := json.Marshal(map[string]any{
		"project_id": round.ProjectID, "round_no": round.RoundNo, "decision": decision,
	})
	if err != nil {
		return fmt.Errorf("marshal proof decision payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key,
			title, body, payload, due_at)
		VALUES ($1,$2,'proof_round',$3,$4,$5,$6,$7,now())
		ON CONFLICT DO NOTHING`,
		tenantID, kind, round.ID, "proof_decision:"+round.ID.String()+":"+decision,
		title, body, payload); err != nil {
		return fmt.Errorf("queue proof decision notification: %w", err)
	}
	return nil
}

// inviteMail and friends are pure so templates can be asserted without a DB.

func inviteMail(d *proofMailData, due string) (string, string) {
	subject := fmt.Sprintf("《%s》审片邀请（第 %d 轮）", d.ProjectTitle, d.RoundNo)
	var b strings.Builder
	fmt.Fprintf(&b, "%s 您好：\n\n", greeting(d.ClientName))
	if d.GalleryTitle != "" {
		fmt.Fprintf(&b, "《%s》已完成初选，现邀请您审片（第 %d 轮）。\n", d.GalleryTitle, d.RoundNo)
	} else {
		fmt.Fprintf(&b, "照片已完成初选，现邀请您审片（第 %d 轮）。\n", d.RoundNo)
	}
	fmt.Fprintf(&b, "\n请于 %s 前打开以下链接，逐张查看、批注或提交修改意见：\n\n%s\n\n", due, d.URL)
	b.WriteString("链接仅供您本人使用，请勿转发。修图回传后可继续复核或提出修改，直至确认通过。\n")
	return subject, b.String()
}

func resubmittedMail(d *proofMailData) (string, string) {
	subject := fmt.Sprintf("《%s》修图版已回传（第 %d 轮）", d.ProjectTitle, d.RoundNo)
	var b strings.Builder
	fmt.Fprintf(&b, "%s 您好：\n\n", greeting(d.ClientName))
	fmt.Fprintf(&b, "第 %d 轮的修图版本已回传，请打开以下链接复核：\n\n%s\n\n", d.RoundNo, d.URL)
	b.WriteString("若确认无误请点击「通过」；本轮仅支持一次修改往返，如需再次调整将开启新一轮审片。\n")
	return subject, b.String()
}

func deadlineMail(d *proofMailData, due string) (string, string) {
	subject := fmt.Sprintf("审片将于 %s 截止", due)
	var b strings.Builder
	fmt.Fprintf(&b, "%s 您好：\n\n", greeting(d.ClientName))
	fmt.Fprintf(&b, "第 %d 轮审片将于 %s 截止，请尽快查看并提交意见：\n\n%s\n", d.RoundNo, due, d.URL)
	return subject, b.String()
}

func greeting(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "您好"
	}
	return name
}
