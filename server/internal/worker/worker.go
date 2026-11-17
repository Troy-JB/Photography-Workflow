package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/archive"
	"photography-workflow/server/internal/delivery"
	"photography-workflow/server/internal/gallery"
	"photography-workflow/server/internal/integration"
	"photography-workflow/server/internal/media"
	"photography-workflow/server/internal/notice"
	"photography-workflow/server/internal/notify"
	"photography-workflow/server/internal/payment"
	"photography-workflow/server/internal/proof"
	"photography-workflow/server/internal/schedule"
	"photography-workflow/server/internal/storage"
	"photography-workflow/server/internal/webhook"
)

const jobTimeout = 30 * time.Minute
const leaseRenewInterval = 1 * time.Minute
const leaseStaleAfter = 3 * time.Minute
const outboxLease = 5 * time.Minute
const defaultPaymentSweepInterval = 1 * time.Minute
const proofSweepInterval = 60 * time.Second

type Options struct {
	ID                   string
	PollInterval         time.Duration
	BatchSize            int
	AlertsWebhookURL     string
	AlertsWebhookSecret  string
	AlertsWebhookTimeout time.Duration
	Reminders            notice.Config
	// PaymentProvider sweeps abandoned orders and settles delayed selection
	// refunds. Nil disables those payment tasks.
	PaymentProvider      payment.Provider
	PaymentSweepInterval time.Duration
	// Media configures thumb/display derivation. A bad watermark font is a
	// boot error, not a runtime surprise.
	Media media.Config
	// GalleryReminderLead queues a gallery_deadline notification this long
	// before the selection closes. Zero disables gallery reminders.
	GalleryReminderLead time.Duration
	// RetouchProvider drives folder hand-off for proof retouch tasks. nil keeps
	// the worker job pipeline but disables the integration sweep.
	RetouchProvider     integration.Provider
	Integration         integration.ServiceConfig
	IntegrationInterval time.Duration
	// ProofRevisionMaxBytes bounds retouched uploads imported by the worker.
	ProofRevisionMaxBytes int64
	// Delivery configures web render, package builds and spool cleanup.
	Delivery delivery.Config
	// Archive configures the snapshot sweep and retention warning defaults.
	Archive archive.Config
	// Email configures SMTP delivery for proof notifications.
	Email notify.EmailConfig
	// PublicBaseURL builds the proof links in reminder mail; Timezone formats
	// due dates for staff and customers.
	PublicBaseURL string
	Timezone      *time.Location
}

type Worker struct {
	pool              *pgxpool.Pool
	store             storage.Storage
	log               *slog.Logger
	opts              Options
	alerts            *alert.Service
	notices           *notice.Service
	pay               payment.Provider
	selectionPayments *payment.Service
	media             *media.Service
	integration       *integration.Service
	proof             *proof.Service
	deliveries        *delivery.Service
	archives          *archive.Service
	email             *notify.Service
	notifier          *notify.ProofNotifier
	httpClient        *http.Client
	noticeClient      *http.Client
}

func New(pool *pgxpool.Pool, store storage.Storage, log *slog.Logger, opts Options) (*Worker, error) {
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.PaymentSweepInterval <= 0 {
		opts.PaymentSweepInterval = defaultPaymentSweepInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 10
	}
	if opts.ID == "" {
		opts.ID = "worker"
	}
	alertCfg := alert.Config{
		WebhookURL:     opts.AlertsWebhookURL,
		WebhookSecret:  opts.AlertsWebhookSecret,
		WebhookTimeout: opts.AlertsWebhookTimeout,
	}
	if alertCfg.WebhookTimeout <= 0 {
		alertCfg.WebhookTimeout = 10 * time.Second
	}
	reminderCfg := opts.Reminders
	if reminderCfg.WebhookTimeout <= 0 {
		reminderCfg.WebhookTimeout = 10 * time.Second
	}
	mediaSvc, err := media.NewService(pool, store, opts.Media, log)
	if err != nil {
		return nil, fmt.Errorf("init media service: %w", err)
	}
	w := &Worker{
		pool: pool, store: store, log: log, opts: opts,
		alerts:       alert.NewService(pool, alertCfg, log),
		notices:      notice.NewService(pool, reminderCfg, log),
		pay:          opts.PaymentProvider,
		media:        mediaSvc,
		httpClient:   &http.Client{Timeout: alertCfg.WebhookTimeout},
		noticeClient: &http.Client{Timeout: reminderCfg.WebhookTimeout},
	}
	if opts.PaymentProvider != nil {
		w.selectionPayments = payment.NewService(pool, opts.PaymentProvider, 0, log)
	}
	// The proof service is always built: revision imports, deadline reminders
	// and the integration sweep share it, even with the manual provider.
	w.proof = proof.NewService(pool, store, nil, proof.Config{
		PublicBaseURL:    opts.PublicBaseURL,
		RevisionMaxBytes: opts.ProofRevisionMaxBytes,
		DefaultWatermark: opts.Media.WatermarkText,
	}, log)
	w.email = notify.NewService(pool, opts.Email, w.alerts, log)
	w.deliveries = delivery.NewService(pool, store, nil, opts.Delivery, log)
	w.deliveries.SetAlerts(w.alerts)
	w.archives = archive.NewService(pool, store, opts.Archive, log)
	w.archives.SetAlerts(w.alerts)
	w.notifier = notify.NewProofNotifier(notify.ProofNotifierConfig{
		PublicBaseURL: opts.PublicBaseURL,
		EmailEnabled:  opts.Email.Enabled,
		Timezone:      opts.Timezone,
	}, log)
	if opts.RetouchProvider != nil {
		if opts.IntegrationInterval <= 0 {
			opts.IntegrationInterval = 30 * time.Second
		}
		w.integration = integration.NewService(pool, opts.RetouchProvider, w.proof, w.alerts,
			opts.Integration, log)
	}
	return w, nil
}

type loopSpec struct {
	name     string
	interval time.Duration
	fn       func(context.Context) (int, error)
}

func (w *Worker) Run(ctx context.Context) error {
	loops := []loopSpec{
		{"jobs", w.opts.PollInterval, w.runJobs},
		{"outbox", w.opts.PollInterval, w.dispatchOutbox},
		{"schedule", w.opts.PollInterval, w.expireHolds},
		{"notifications", w.opts.PollInterval, w.dispatchNotifications},
		{"email", w.opts.PollInterval, w.dispatchEmail},
		{"proof-deadline", proofSweepInterval, w.sweepProofDeadlines},
		{"gallery-sweep", gallerySweepInterval, w.sweepGalleries},
		{"selection-refunds", gallerySweepInterval, w.sweepSelectionRefunds},
		{"delivery-temp", time.Hour, cleanupDeliveryTemp(w.deliveries)},
		{"archive-sweep", w.archives.SweepInterval(), w.sweepArchives},
	}
	if w.pay != nil {
		loops = append(loops, loopSpec{"payment-sweep", w.opts.PaymentSweepInterval, w.sweepPaymentOrders})
	}
	if w.integration != nil {
		loops = append(loops, loopSpec{"integrations", w.opts.IntegrationInterval, w.sweepIntegrations})
	}
	var wg sync.WaitGroup
	wg.Add(len(loops))
	for _, l := range loops {
		go func(l loopSpec) {
			defer wg.Done()
			w.loop(ctx, l.name, l.interval, l.fn)
		}(l)
	}
	wg.Wait()
	return nil
}

// sweepPaymentOrders closes reservations that never reached the payment
// channel. It runs on the worker role because it must scan every tenant; the
// API role is RLS-scoped to one tenant at a time.
func (w *Worker) sweepPaymentOrders(ctx context.Context) (int, error) {
	return payment.SweepCreatingOrders(ctx, w.pool, w.pay, w.alerts,
		payment.OrderCreateGrace, 50, w.log)
}

func (w *Worker) sweepSelectionRefunds(ctx context.Context) (int, error) {
	if w.selectionPayments == nil {
		return 0, nil
	}
	rows, err := w.pool.Query(ctx, `SELECT tenant_id,id FROM galleries
		WHERE selection_edit_until IS NOT NULL AND selection_edit_until <= now()
		ORDER BY selection_edit_until LIMIT $1`, gallerySweepBatch)
	if err != nil {
		return 0, fmt.Errorf("scan galleries with expired selection edit windows: %w", err)
	}
	type candidate struct{ tenantID, galleryID uuid.UUID }
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.tenantID, &c.galleryID); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	requested := 0
	for _, c := range candidates {
		tasks, err := gallery.DueSelectionRefunds(ctx, w.pool, c.tenantID, c.galleryID)
		if err != nil {
			return requested, fmt.Errorf("load selection refunds for gallery %s: %w", c.galleryID, err)
		}
		for _, task := range tasks {
			if _, err := w.selectionPayments.CreateRefund(ctx, c.tenantID, uuid.Nil, "", payment.CreateRefundInput{PaymentID: task.PaymentID, AmountCents: task.AmountCents, Reason: "套餐选片数量调整退款"}); err != nil {
				return requested, err
			}
			requested++
		}
	}
	return requested, nil
}

// expireHolds releases lapsed booking holds. It runs on this worker's pool,
// which maps to the pwf_worker role with grants on bookings and resource_blocks;
// pending reminders for the expired booking are cancelled in the same transaction.
func (w *Worker) expireHolds(ctx context.Context) (int, error) {
	return schedule.ExpireHolds(ctx, w.pool, 100, w.notices)
}

// dispatchNotifications enqueues due reminders as notice.deliver jobs. Delivery
// itself happens on the jobs loop so retries and dead-lettering are shared.
func (w *Worker) dispatchNotifications(ctx context.Context) (int, error) {
	return w.notices.DispatchDue(ctx, w.pool, 100)
}

// dispatchEmail turns pending queued mail into email.send jobs so delivery
// shares the same retry and dead-lettering path as every other job.
func (w *Worker) dispatchEmail(ctx context.Context) (int, error) {
	return w.email.DispatchDue(ctx, 100)
}

// sweepIntegrations exports queued retouch jobs, imports retouched files,
// reconciles delivery and raises overdue alerts.
func (w *Worker) sweepIntegrations(ctx context.Context) (int, error) {
	return w.integration.Sweep(ctx)
}

func (w *Worker) loop(ctx context.Context, name string, interval time.Duration, fn func(context.Context) (int, error)) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker loop stopped", "loop", name)
			return
		default:
		}

		n, err := fn(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.log.Warn("worker loop error", "loop", name, "err", err)
		}
		if n > 0 {
			continue
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

type claimedJob struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Kind        string
	SubjectType string
	SubjectID   *uuid.UUID
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
	LeaseToken  string
}

func (w *Worker) runJobs(ctx context.Context) (int, error) {
	if _, err := w.pool.Exec(ctx, `
		UPDATE jobs SET status = 'queued', locked_at = NULL, locked_by = '',
			lease_token = '', last_error = 'requeued after stalled worker', updated_at = now()
		WHERE status = 'running' AND locked_at < now() - interval '3 minutes'`); err != nil {
		return 0, fmt.Errorf("requeue stale jobs: %w", err)
	}

	rows, err := w.pool.Query(ctx, `
		WITH claimed AS (
			SELECT id FROM jobs
			WHERE status = 'queued' AND run_after <= now()
			ORDER BY run_after
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs j
		SET status = 'running', attempts = j.attempts + 1, locked_at = now(),
			locked_by = $2, lease_token = gen_random_uuid()::text, updated_at = now()
		FROM claimed
		WHERE j.id = claimed.id
		RETURNING j.id, j.tenant_id, j.kind, j.subject_type, j.subject_id, j.payload,
			j.attempts, j.max_attempts, j.lease_token`, w.opts.BatchSize, w.opts.ID)
	if err != nil {
		return 0, fmt.Errorf("claim jobs: %w", err)
	}
	var claimed []claimedJob
	for rows.Next() {
		var j claimedJob
		if err := rows.Scan(&j.ID, &j.TenantID, &j.Kind, &j.SubjectType, &j.SubjectID,
			&j.Payload, &j.Attempts, &j.MaxAttempts, &j.LeaseToken); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan job: %w", err)
		}
		claimed = append(claimed, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("claim jobs: %w", err)
	}

	for _, job := range claimed {
		w.executeJob(ctx, job)
	}
	return len(claimed), nil
}

func (w *Worker) executeJob(parent context.Context, job claimedJob) {
	ctx, cancel := context.WithTimeout(parent, jobTimeout)
	defer cancel()
	leaseLost := make(chan struct{}, 1)
	leaseDone := make(chan struct{})
	go w.renewLease(ctx, job, leaseLost, leaseDone)

	start := time.Now()
	result, err := w.handle(ctx, job)
	cancel()
	<-leaseDone
	select {
	case <-leaseLost:
		w.log.Error("job lease lost; result discarded", "job_id", job.ID, "kind", job.Kind)
		return
	default:
	}
	if err != nil {
		if parent.Err() != nil {
			// Shutting down: put the job back without a terminal failure.
			if _, uerr := w.pool.Exec(context.Background(), `
				UPDATE jobs SET status = 'queued', locked_at = NULL, locked_by = '', lease_token = '', updated_at = now()
				WHERE id = $1 AND locked_by = $2 AND lease_token = $3`, job.ID, w.opts.ID, job.LeaseToken); uerr != nil {
				w.log.Warn("requeue job on shutdown", "job_id", job.ID, "err", uerr)
			}
			return
		}
		w.failJob(job, err)
		w.log.Error("job failed", "job_id", job.ID, "kind", job.Kind,
			"attempts", job.Attempts, "duration_ms", time.Since(start).Milliseconds(), "err", err)
		return
	}

	raw, _ := json.Marshal(result)
	tag, err := w.pool.Exec(context.Background(), `
		UPDATE jobs SET status = 'succeeded', result = $2, finished_at = now(),
			locked_at = NULL, locked_by = '', lease_token = '', updated_at = now()
		WHERE id = $1 AND locked_by = $3 AND lease_token = $4`, job.ID, raw, w.opts.ID, job.LeaseToken)
	if err != nil {
		w.log.Error("mark job succeeded", "job_id", job.ID, "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		// Another worker reclaimed the job while this one was finishing; the
		// result belongs to nobody and the new lease owns the row.
		w.log.Warn("job lease lost before success could be recorded", "job_id", job.ID, "kind", job.Kind)
		return
	}
	w.log.Info("job succeeded", "job_id", job.ID, "kind", job.Kind,
		"attempts", job.Attempts, "duration_ms", time.Since(start).Milliseconds())
}

func (w *Worker) failJob(job claimedJob, cause error) {
	dead := job.Attempts >= job.MaxAttempts
	tag, err := w.pool.Exec(context.Background(), `
		UPDATE jobs SET
			status = CASE WHEN $2 THEN 'failed' ELSE 'queued' END,
			run_after = now() + make_interval(secs => least($3, 300)),
			last_error = $4,
			locked_at = NULL,
			locked_by = '',
			lease_token = '',
			finished_at = CASE WHEN $2 THEN now() ELSE NULL END,
			updated_at = now()
		WHERE id = $1 AND locked_by = $5 AND lease_token = $6`, job.ID, dead, job.Attempts*5, cause.Error(), w.opts.ID, job.LeaseToken)
	if err != nil {
		w.log.Error("mark job failed", "job_id", job.ID, "err", err)
		return
	}
	if tag.RowsAffected() == 0 {
		// The lease was reclaimed before this failure was recorded. The job
		// will be retried under a new lease, so the alert/notification must not
		// be marked dead here; whichever worker owns the retry decides.
		w.log.Warn("job lease lost before failure could be recorded; notification state left untouched",
			"job_id", job.ID, "kind", job.Kind)
		return
	}
	if dead && job.Kind == alert.NotifyJobKind && job.SubjectID != nil {
		if err := w.alerts.RecordNotify(context.Background(), job.TenantID, *job.SubjectID, "dead", cause.Error()); err != nil {
			w.log.Error("mark alert notification dead", "alert_id", *job.SubjectID, "err", err)
		}
	}
	if dead && job.Kind == notice.JobKind && job.SubjectID != nil {
		if err := w.notices.MarkDead(context.Background(), job.TenantID, *job.SubjectID, cause.Error()); err != nil {
			w.log.Error("mark reminder dead", "notification_id", *job.SubjectID, "err", err)
		}
	}
	if dead && job.Kind == notify.JobKind && job.SubjectID != nil {
		if err := w.email.MarkDead(context.Background(), job.TenantID, *job.SubjectID, cause.Error()); err != nil {
			w.log.Error("mark email dead", "message_id", *job.SubjectID, "err", err)
		}
	}
	switch job.Kind {
	case delivery.JobBuild, delivery.JobRenderWeb, delivery.JobPackage:
		if dead {
			w.failDeliveryBuild(job, cause)
		}
	case archive.JobVerify, archive.JobVerifyItem, archive.JobDeleteItem:
		if dead {
			w.failArchiveJob(job, cause)
		}
	}
}

// failArchiveJob records a permanent archive failure so the archive never stays
// stuck in verifying/deleting and the owner can retry.
func (w *Worker) failArchiveJob(job claimedJob, cause error) {
	var payload archive.VerifyPayload
	if len(job.Payload) == 0 || json.Unmarshal(job.Payload, &payload) != nil || payload.ArchiveID == uuid.Nil {
		w.log.Warn("archive job died without a usable payload", "job_id", job.ID, "kind", job.Kind)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.archives.FailJob(ctx, job.TenantID, payload.ArchiveID, job.Kind, cause.Error()); err != nil {
		w.log.Error("mark archive failed", "archive_id", payload.ArchiveID, "err", err)
	}
}

// failDeliveryBuild records a permanent build failure on the delivery and
// raises an operator alert. Only the build version in the payload is touched,
// so a concurrent rebuild is not marked failed by a stale job.
func (w *Worker) failDeliveryBuild(job claimedJob, cause error) {
	var payload delivery.BuildPayload
	if len(job.Payload) == 0 || json.Unmarshal(job.Payload, &payload) != nil || payload.DeliveryID == uuid.Nil {
		w.log.Warn("delivery job died without a usable payload", "job_id", job.ID, "kind", job.Kind)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.deliveries.FailBuild(ctx, job.TenantID, payload.DeliveryID, payload.BuildVersion,
		fmt.Sprintf("%s failed: %s", job.Kind, cause.Error())); err != nil {
		w.log.Error("mark delivery build failed", "delivery_id", payload.DeliveryID, "err", err)
	}
}

func (w *Worker) renewLease(ctx context.Context, job claimedJob, lost chan<- struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(leaseRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var rows int64
			err := w.pool.QueryRow(renewCtx, `
				UPDATE jobs SET locked_at = now(), updated_at = now()
				WHERE id = $1 AND locked_by = $2 AND lease_token = $3 AND status = 'running'
				RETURNING 1`, job.ID, w.opts.ID, job.LeaseToken).Scan(&rows)
			cancel()
			if err == nil && rows == 1 {
				continue
			}
			if err == nil && rows == 0 {
				select {
				case lost <- struct{}{}:
				default:
				}
				return
			}
			w.log.Warn("renew job lease failed", "job_id", job.ID, "err", err)
		}
	}
}

func (w *Worker) handle(ctx context.Context, job claimedJob) (any, error) {
	switch job.Kind {
	case gallery.SelectionRefundJobKind:
		if w.selectionPayments == nil {
			return nil, fmt.Errorf("selection refunds require a configured payment provider")
		}
		if job.SubjectID == nil {
			return nil, fmt.Errorf("selection refund needs a gallery subject")
		}
		tasks, err := gallery.DueSelectionRefunds(ctx, w.pool, job.TenantID, *job.SubjectID)
		if err != nil {
			return nil, err
		}
		created := 0
		for _, task := range tasks {
			if _, err := w.selectionPayments.CreateRefund(ctx, job.TenantID, uuid.Nil, "", payment.CreateRefundInput{
				PaymentID: task.PaymentID, AmountCents: task.AmountCents, Reason: "套餐选片数量调整退款",
			}); err != nil {
				return nil, err
			}
			created++
		}
		return map[string]int{"refunds_requested": created}, nil
	case "ingest.verify":
		if job.SubjectID == nil {
			return nil, fmt.Errorf("ingest.verify needs a batch subject")
		}
		return w.verifyBatch(ctx, job.TenantID, *job.SubjectID)
	case media.JobKind:
		return w.deriveAsset(ctx, job)
	case media.RevisionJobKind:
		return w.deriveRevision(ctx, job)
	case alert.NotifyJobKind:
		if job.SubjectID == nil {
			return nil, fmt.Errorf("notify.webhook needs an alert subject")
		}
		return w.notifyAlert(ctx, job.TenantID, *job.SubjectID)
	case notice.JobKind:
		if job.SubjectID == nil {
			return nil, fmt.Errorf("notice.deliver needs a notification subject")
		}
		return w.deliverNotification(ctx, job.TenantID, *job.SubjectID)
	case notify.JobKind:
		if job.SubjectID == nil {
			return nil, fmt.Errorf("email.send needs a message subject")
		}
		return w.email.Deliver(ctx, job.TenantID, *job.SubjectID)
	case delivery.JobBuild:
		return w.planDeliveryBuild(ctx, job)
	case delivery.JobRenderWeb:
		return w.renderDeliveryWeb(ctx, job)
	case delivery.JobPackage:
		return w.buildDeliveryPackage(ctx, job)
	case archive.JobVerify:
		return w.planArchiveVerify(ctx, job)
	case archive.JobVerifyItem:
		return w.verifyArchiveItem(ctx, job)
	case archive.JobDeleteItem:
		return w.deleteArchiveItem(ctx, job)
	default:
		return nil, fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

// sweepArchives auto-creates archives for archived projects and raises the
// one-time retention warning. It never deletes objects.
func (w *Worker) sweepArchives(ctx context.Context) (int, error) {
	return w.archives.Sweep(ctx)
}

func (w *Worker) planArchiveVerify(ctx context.Context, job claimedJob) (any, error) {
	var payload archive.VerifyPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode %s payload: %w", job.Kind, err)
		}
	}
	if payload.ArchiveID == uuid.Nil && job.SubjectID != nil {
		payload.ArchiveID = *job.SubjectID
	}
	if payload.ArchiveID == uuid.Nil {
		return nil, fmt.Errorf("%s needs an archive", job.Kind)
	}
	return w.archives.PlanVerify(ctx, job.TenantID, payload.ArchiveID)
}

func (w *Worker) verifyArchiveItem(ctx context.Context, job claimedJob) (any, error) {
	payload, err := archiveItemPayload(job)
	if err != nil {
		return nil, err
	}
	return w.archives.VerifyItem(ctx, job.TenantID, payload.ArchiveID, payload.ItemID)
}

func (w *Worker) deleteArchiveItem(ctx context.Context, job claimedJob) (any, error) {
	payload, err := archiveItemPayload(job)
	if err != nil {
		return nil, err
	}
	return w.archives.DeleteItem(ctx, job.TenantID, payload.ArchiveID, payload.ItemID)
}

func archiveItemPayload(job claimedJob) (archive.ItemPayload, error) {
	var payload archive.ItemPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return payload, fmt.Errorf("decode %s payload: %w", job.Kind, err)
		}
	}
	if payload.ItemID == uuid.Nil && job.SubjectID != nil {
		payload.ItemID = *job.SubjectID
	}
	if payload.ArchiveID == uuid.Nil || payload.ItemID == uuid.Nil {
		return payload, fmt.Errorf("%s needs an archive and an item", job.Kind)
	}
	return payload, nil
}

// cleanupDeliveryTemp removes stale spools left by a crashed package build.
func cleanupDeliveryTemp(svc *delivery.Service) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return svc.CleanupTemp(time.Now())
	}
}

func (w *Worker) deliveryPayload(job claimedJob) (delivery.BuildPayload, error) {
	var payload delivery.BuildPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return payload, fmt.Errorf("decode %s payload: %w", job.Kind, err)
		}
	}
	if payload.DeliveryID == uuid.Nil && job.SubjectID != nil {
		payload.DeliveryID = *job.SubjectID
	}
	if payload.DeliveryID == uuid.Nil {
		return payload, fmt.Errorf("%s needs a delivery", job.Kind)
	}
	return payload, nil
}

// planDeliveryBuild snapshots items and queues one render job per asset.
func (w *Worker) planDeliveryBuild(ctx context.Context, job claimedJob) (any, error) {
	payload, err := w.deliveryPayload(job)
	if err != nil {
		return nil, err
	}
	return w.deliveries.PlanBuild(ctx, job.TenantID, payload.DeliveryID, payload.BuildVersion)
}

// renderDeliveryWeb renders one 2048px JPEG for the web package.
func (w *Worker) renderDeliveryWeb(ctx context.Context, job claimedJob) (any, error) {
	payload, err := w.deliveryPayload(job)
	if err != nil {
		return nil, err
	}
	if payload.ItemID == uuid.Nil {
		return nil, fmt.Errorf("delivery.render_web needs an item")
	}
	return w.deliveries.RenderWebItem(ctx, job.TenantID, payload.DeliveryID, payload.ItemID,
		payload.BuildVersion)
}

// buildDeliveryPackage writes one ZIP and finalizes the delivery when both
// specs are ready.
func (w *Worker) buildDeliveryPackage(ctx context.Context, job claimedJob) (any, error) {
	payload, err := w.deliveryPayload(job)
	if err != nil {
		return nil, err
	}
	if payload.PackageID == uuid.Nil {
		return nil, fmt.Errorf("delivery.package needs a package")
	}
	return w.deliveries.BuildPackage(ctx, job.TenantID, payload.DeliveryID, payload.PackageID,
		payload.BuildVersion)
}

// deriveAsset renders the gallery previews of one asset. The payload wins over
// the subject id because the batch endpoint may re-enqueue with force=true.
func (w *Worker) deriveAsset(ctx context.Context, job claimedJob) (any, error) {
	var payload media.JobPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode media.derive payload: %w", err)
		}
	}
	assetID := payload.AssetID
	if assetID == uuid.Nil && job.SubjectID != nil {
		assetID = *job.SubjectID
	}
	if assetID == uuid.Nil {
		return nil, fmt.Errorf("media.derive needs an asset")
	}
	res, err := w.media.Derive(ctx, job.TenantID, assetID, payload.Watermark, payload.Force)
	if err != nil {
		return nil, err
	}
	w.log.Info("asset derivatives ready", "asset_id", assetID, "tenant_id", job.TenantID,
		"source", res.Source, "skipped", res.Skipped)
	return res, nil
}

// deriveRevision renders the previews of one retouched version. It never
// touches asset_derivatives, so the gallery preview of the original survives.
func (w *Worker) deriveRevision(ctx context.Context, job claimedJob) (any, error) {
	var payload media.RevisionPayload
	if len(job.Payload) > 0 {
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode media.derive_revision payload: %w", err)
		}
	}
	revisionID := payload.RevisionID
	if revisionID == uuid.Nil && job.SubjectID != nil {
		revisionID = *job.SubjectID
	}
	if revisionID == uuid.Nil {
		return nil, fmt.Errorf("media.derive_revision needs a revision")
	}
	res, err := w.media.DeriveRevision(ctx, job.TenantID, revisionID, payload.Watermark)
	if err != nil {
		w.raiseRevisionAlert(ctx, job.TenantID, revisionID, err)
		return nil, err
	}
	w.log.Info("revision derivatives ready", "revision_id", revisionID, "asset_id", res.AssetID,
		"tenant_id", job.TenantID, "revision_no", res.RevisionNo, "skipped", res.Skipped)
	return res, nil
}

// notifyAlert delivers one payment-alert webhook. The alert row is the source
// of truth: delivery state advances only after the receiver answers.
func (w *Worker) notifyAlert(ctx context.Context, tenantID, alertID uuid.UUID) (any, error) {
	a, err := w.alerts.LoadForDelivery(ctx, tenantID, alertID)
	if err != nil {
		return nil, err
	}
	switch a.NotifyStatus {
	case "sent", "skipped":
		return map[string]any{"status": a.NotifyStatus}, nil
	}
	if !w.alerts.WebhookEnabled() {
		if err := w.alerts.RecordNotify(ctx, tenantID, alertID, "skipped", ""); err != nil {
			return nil, err
		}
		return map[string]any{"status": "skipped"}, nil
	}

	err = alert.SendWebhook(ctx, w.httpClient, alert.Config{
		WebhookURL:     w.opts.AlertsWebhookURL,
		WebhookSecret:  w.opts.AlertsWebhookSecret,
		WebhookTimeout: w.opts.AlertsWebhookTimeout,
	}, alert.NewNotification(a))
	if err == nil {
		if rerr := w.alerts.RecordNotify(ctx, tenantID, alertID, "sent", ""); rerr != nil {
			return nil, rerr
		}
		return map[string]any{"status": "sent"}, nil
	}
	if errors.Is(err, alert.ErrPermanentDelivery) {
		if rerr := w.alerts.RecordNotify(ctx, tenantID, alertID, "failed", err.Error()); rerr != nil {
			return nil, rerr
		}
		w.log.Warn("alert webhook rejected permanently", "alert_id", alertID, "err", err)
		return map[string]any{"status": "failed"}, nil
	}
	// Retryable failure: keep the row pending and let the job backoff retry.
	// failJob flips the row to dead once attempts are exhausted.
	if rerr := w.alerts.RecordNotify(ctx, tenantID, alertID, "pending", err.Error()); rerr != nil {
		w.log.Error("record alert notification failure", "alert_id", alertID, "err", rerr)
	}
	return nil, err
}

// deliverNotification sends one due reminder after re-checking that it still
// describes the booking. Stale reminders are recorded as skipped, never sent.
func (w *Worker) deliverNotification(ctx context.Context, tenantID, notificationID uuid.UUID) (any, error) {
	n, err := w.notices.LoadForDelivery(ctx, tenantID, notificationID)
	if err != nil {
		return nil, err
	}
	switch n.Status {
	case notice.StatusSent, notice.StatusSkipped, notice.StatusFailed,
		notice.StatusDead, notice.StatusCancelled:
		return map[string]any{"status": n.Status}, nil
	}
	state, err := w.notices.DeliveryState(ctx, tenantID, n)
	if err != nil {
		return nil, err
	}
	// Gallery events have no booking state; they are stale only once the album
	// stops being open (revoked, expired or deleted).
	if n.SubjectType == "gallery" {
		open, err := w.galleryOpen(ctx, tenantID, n.SubjectID)
		if err != nil {
			return nil, err
		}
		if !open {
			if err := w.notices.RecordDelivery(ctx, tenantID, notificationID, notice.StatusSkipped,
				"gallery is no longer open"); err != nil {
				return nil, err
			}
			return map[string]any{"status": notice.StatusSkipped}, nil
		}
	}
	if !notice.Applicable(n, state, time.Now()) {
		if err := w.notices.RecordDelivery(ctx, tenantID, notificationID, notice.StatusSkipped,
			"booking no longer matches this reminder"); err != nil {
			return nil, err
		}
		return map[string]any{"status": notice.StatusSkipped}, nil
	}
	status, err := w.notices.Deliver(ctx, w.noticeClient, n, state, time.Now())
	if err == nil {
		if rerr := w.notices.RecordDelivery(ctx, tenantID, notificationID, status, ""); rerr != nil {
			return nil, rerr
		}
		return map[string]any{"status": status}, nil
	}
	if errors.Is(err, webhook.ErrPermanentDelivery) {
		if rerr := w.notices.RecordDelivery(ctx, tenantID, notificationID, notice.StatusFailed, err.Error()); rerr != nil {
			return nil, rerr
		}
		w.log.Warn("reminder webhook rejected permanently", "notification_id", notificationID, "err", err)
		return map[string]any{"status": notice.StatusFailed}, nil
	}
	// Retryable failure: keep the row pending and let the job back off.
	// failJob flips it to dead once attempts are exhausted.
	if rerr := w.notices.RecordDelivery(ctx, tenantID, notificationID, notice.StatusPending, err.Error()); rerr != nil {
		w.log.Error("record reminder delivery failure", "notification_id", notificationID, "err", rerr)
	}
	return nil, err
}

type verifyResult struct {
	FileCount int   `json:"file_count"`
	Bytes     int64 `json:"bytes"`
	Verified  int   `json:"verified"`
	Missing   int   `json:"missing"`
	Corrupt   int   `json:"corrupt"`
}

// verifyBatch enforces the asset contract: an asset is verified only after the
// stored object's size and SHA-256 both match the registered metadata.
func (w *Worker) verifyBatch(ctx context.Context, tenantID, batchID uuid.UUID) (*verifyResult, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT id, original_key, content_hash, size_bytes, status
		FROM assets
		WHERE tenant_id = $1 AND ingest_batch_id = $2
		ORDER BY id`, tenantID, batchID)
	if err != nil {
		return nil, fmt.Errorf("load batch assets: %w", err)
	}
	type assetRow struct {
		ID          uuid.UUID
		OriginalKey string
		ContentHash string
		SizeBytes   int64
		Status      string
	}
	var assets []assetRow
	for rows.Next() {
		var a assetRow
		if err := rows.Scan(&a.ID, &a.OriginalKey, &a.ContentHash, &a.SizeBytes, &a.Status); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		assets = append(assets, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load batch assets: %w", err)
	}

	out := &verifyResult{}
	for _, a := range assets {
		out.FileCount++
		out.Bytes += a.SizeBytes

		if isVerified(a.Status) {
			out.Verified++
			continue
		}

		status, err := w.checkAsset(ctx, a.OriginalKey, a.ContentHash, a.SizeBytes)
		if err != nil {
			return nil, fmt.Errorf("verify asset %s: %w", a.ID, err)
		}
		switch status {
		case "verified":
			out.Verified++
		case "missing":
			out.Missing++
		case "corrupt":
			out.Corrupt++
		}
		if _, err := w.pool.Exec(ctx, `
			UPDATE assets SET status = $3,
				verified_at = CASE WHEN $3 = 'verified' THEN now() ELSE verified_at END,
				updated_at = now()
			WHERE id = $1 AND tenant_id = $2`, a.ID, tenantID, status); err != nil {
			return nil, fmt.Errorf("update asset status: %w", err)
		}
	}

	finalStatus := "partial"
	if out.FileCount > 0 && out.Verified == out.FileCount {
		finalStatus = "completed"
	} else if out.Verified == 0 && out.Missing+out.Corrupt == out.FileCount {
		finalStatus = "failed"
	}
	if _, err := w.pool.Exec(ctx, `
		UPDATE ingest_batches SET file_count = $3, bytes = $4, status = $5,
			finished_at = CASE WHEN $5 IN ('completed','partial','failed') THEN now() ELSE finished_at END
		WHERE id = $1 AND tenant_id = $2 AND status = 'verifying'`,
		batchID, tenantID, out.FileCount, out.Bytes, finalStatus); err != nil {
		return nil, fmt.Errorf("update batch summary: %w", err)
	}
	return out, nil
}

func (w *Worker) checkAsset(ctx context.Context, key, wantHash string, wantSize int64) (string, error) {
	if key == "" {
		return "missing", nil
	}
	size, err := w.store.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "missing", nil
		}
		return "", err
	}
	if size != wantSize {
		return "corrupt", nil
	}

	rc, err := w.store.Open(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "missing", nil
		}
		return "", err
	}
	defer rc.Close()

	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "missing", nil
		}
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != wantHash {
		return "corrupt", nil
	}
	return "verified", nil
}

func isVerified(status string) bool {
	switch status {
	case "verified", "derivative_ready", "culled", "selected", "retouching",
		"proofing", "approved", "delivered", "archived":
		return true
	}
	return false
}

type outboxEvent struct {
	ID         int64
	TenantID   uuid.UUID
	EventType  string
	Attempts   int
	Aggregate  string
	ObjectID   string
	LeaseToken string
}

// dispatchOutbox delivers pending events at least once. A claim writes a lease
// token; only the holder may mark the event sent. When a worker overruns its
// lease, another worker reclaims the event and the stale holder's completion
// is refused, so future subscribers cannot be acknowledged twice by a worker
// that no longer owns the event.
func (w *Worker) dispatchOutbox(ctx context.Context) (int, error) {
	if _, err := w.pool.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'retrying', available_at = now(), locked_by = '', lease_token = '',
			last_error = 'reclaimed after lease expiry'
		WHERE status = 'processing' AND available_at < now()`); err != nil {
		return 0, fmt.Errorf("reclaim stalled outbox events: %w", err)
	}

	rows, err := w.pool.Query(ctx, `
		WITH claimed AS (
			SELECT id FROM outbox_events
			WHERE status IN ('pending','retrying') AND available_at <= now()
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events e
		SET status = 'processing', attempts = e.attempts + 1,
			available_at = now() + make_interval(secs => $3),
			locked_by = $2, lease_token = gen_random_uuid()::text
		FROM claimed
		WHERE e.id = claimed.id
		RETURNING e.id, e.tenant_id, e.event_type, e.attempts, e.aggregate_type, e.aggregate_id,
			e.lease_token`,
		w.opts.BatchSize, w.opts.ID, outboxLease.Seconds())
	if err != nil {
		return 0, fmt.Errorf("claim outbox events: %w", err)
	}
	var events []outboxEvent
	for rows.Next() {
		var e outboxEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.EventType, &e.Attempts, &e.Aggregate,
			&e.ObjectID, &e.LeaseToken); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox event: %w", err)
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("claim outbox events: %w", err)
	}

	for _, e := range events {
		// M0 has no external subscribers; delivery is a structured log entry.
		w.log.Info("outbox event delivered",
			"event_id", e.ID, "event_type", e.EventType, "aggregate", e.Aggregate,
			"object_id", e.ObjectID, "tenant_id", e.TenantID, "attempts", e.Attempts)
		tag, err := w.pool.Exec(ctx, `
			UPDATE outbox_events
			SET status = 'sent', processed_at = now(), last_error = '', locked_by = '', lease_token = ''
			WHERE id = $1 AND status = 'processing' AND lease_token = $2`, e.ID, e.LeaseToken)
		if err != nil {
			w.log.Error("mark outbox event sent", "event_id", e.ID, "err", err)
			continue
		}
		if tag.RowsAffected() == 0 {
			// The lease expired and the event was reclaimed while delivering.
			// Do not acknowledge the event on behalf of the new lease owner.
			w.log.Warn("outbox lease lost; delivery not acknowledged", "event_id", e.ID,
				"event_type", e.EventType)
		}
	}
	return len(events), nil
}
