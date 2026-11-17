package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

type smokeConfig struct {
	Base             string
	Email            string
	Password         string
	Title            string
	Size             int
	Bulk             int
	ESignSecret      string
	PaymentSecret    string
	PaymentMerchant  string
	JobTimeout       time.Duration
	AlertListener    string
	AlertSecret      string
	ReminderSecret   string
	ReminderLead     time.Duration
	ScheduleTimezone string
	ScheduleHoldTTL  time.Duration
	BookingRace      int
	StaffEmail       string
	StaffPassword    string
	MailDir          string
	DatabaseURL      string
}

type client struct {
	base       string
	token      string
	http       *http.Client
	jobTimeout time.Duration
	alerts     *alertCapture
}

type assetMeta struct {
	ID          uuid.UUID `json:"id"`
	OriginalKey string    `json:"original_key"`
	Status      string    `json:"status"`
	SizeBytes   int64     `json:"size_bytes"`
}

type uploadPlan struct {
	Mode      string `json:"mode"`
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	UploadID  string `json:"upload_id"`
	PartSize  int64  `json:"part_size"`
	Parts     []struct {
		PartNumber int    `json:"part_number"`
		URL        string `json:"url"`
	} `json:"parts"`
}

type completePart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

type registerResult struct {
	Accepted   int `json:"accepted"`
	Inserted   int `json:"inserted"`
	Duplicated int `json:"duplicated"`
	Assets     []struct {
		AssetID uuid.UUID `json:"asset_id"`
		Status  string    `json:"status"`
	} `json:"assets"`
}

type finalizeJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result struct {
		FileCount int   `json:"file_count"`
		Bytes     int64 `json:"bytes"`
		Verified  int   `json:"verified"`
		Missing   int   `json:"missing"`
		Corrupt   int   `json:"corrupt"`
	} `json:"result"`
	LastError string `json:"last_error"`
}

func main() {
	var cfg smokeConfig
	flag.StringVar(&cfg.Base, "base", "http://localhost:8080", "API base URL")
	flag.StringVar(&cfg.Email, "email", "owner@example.com", "login email")
	flag.StringVar(&cfg.Password, "password", "photographer123", "login password")
	flag.StringVar(&cfg.Title, "title", "Smoke shoot", "project title")
	flag.IntVar(&cfg.Size, "size", 256<<10, "fixture size in bytes; >= 32MiB exercises multipart")
	flag.IntVar(&cfg.Bulk, "bulk", 500, "files registered in one batch for the bulk check; 0 skips it")
	flag.StringVar(&cfg.ESignSecret, "esign-secret", "dev-only-esign-secret-change-me", "sandbox e-sign callback HMAC secret")
	flag.StringVar(&cfg.PaymentSecret, "payment-secret", "dev-only-payment-secret-change-me", "sandbox payment callback HMAC secret")
	flag.StringVar(&cfg.PaymentMerchant, "payment-merchant", "sandbox-merchant-001", "sandbox payment merchant id")
	flag.DurationVar(&cfg.JobTimeout, "job-timeout", 90*time.Second, "how long to wait for an async job")
	flag.StringVar(&cfg.AlertListener, "alert-listener", "", "optional host:port to receive alert webhooks on")
	flag.StringVar(&cfg.AlertSecret, "alert-secret", "dev-only-alerts-secret-change-me", "HMAC secret expected on alert webhooks")
	flag.StringVar(&cfg.ReminderSecret, "reminder-secret", "dev-only-reminders-secret-change-me", "HMAC secret expected on reminder webhooks")
	flag.DurationVar(&cfg.ReminderLead, "reminder-lead", 0, "shoot reminder lead the API runs with; >0 waits for webhook delivery")
	flag.StringVar(&cfg.ScheduleTimezone, "schedule-timezone", "Asia/Shanghai", "timezone availability rules are interpreted in")
	flag.DurationVar(&cfg.ScheduleHoldTTL, "schedule-hold-ttl", 30*time.Minute, "hold TTL the API runs with; expiry is exercised when <= 30s")
	flag.IntVar(&cfg.BookingRace, "booking-race", 20, "concurrent holds competing for one slot; 0 skips")
	flag.StringVar(&cfg.StaffEmail, "staff-email", "", "optional staff account used to prove the proof gate rejects non-owners")
	flag.StringVar(&cfg.StaffPassword, "staff-password", "photographer123", "password for -staff-email")
	flag.StringVar(&cfg.MailDir, "mail-dir", "", "optional mailsink directory to verify proof email delivery")
	flag.StringVar(&cfg.DatabaseURL, "database-url", "", "optional Postgres DSN for email queue and audit assertions")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "smoke: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg smokeConfig) error {
	c := &client{base: cfg.Base, http: &http.Client{Timeout: 120 * time.Second}, jobTimeout: cfg.JobTimeout}
	if c.jobTimeout <= 0 {
		c.jobTimeout = 90 * time.Second
	}

	step("health")
	var health map[string]string
	if err := c.do(http.MethodGet, "/healthz", "", nil, &health); err != nil {
		return err
	}
	fmt.Printf("  db=%s storage=%s\n", health["db"], health["storage"])

	step("metrics")
	var metrics string
	if err := c.doText("/metrics", &metrics); err != nil {
		return err
	}
	if !strings.Contains(metrics, "http_requests_total") {
		return fmt.Errorf("metrics endpoint missing http_requests_total")
	}
	fmt.Println("  prometheus text exposition present")
	if err := runRegistrationSmoke(c); err != nil {
		return err
	}

	step("login")
	var session struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.do(http.MethodPost, "/api/v1/auth/login", "",
		map[string]string{"email": cfg.Email, "password": cfg.Password}, &session); err != nil {
		return err
	}
	c.token = session.AccessToken
	fmt.Println("  token acquired")

	step("alert webhook listener")
	if cfg.AlertListener != "" {
		capture, stop, err := startAlertListener(cfg.AlertListener, cfg.AlertSecret, cfg.ReminderSecret)
		if err != nil {
			return err
		}
		c.alerts = capture
		defer func() { _ = stop() }()
		fmt.Printf("  listening on %s (alerts + reminders)\n", cfg.AlertListener)
	} else {
		fmt.Println("  disabled (pass -alert-listener to verify webhook delivery)")
	}

	step("authentication guard")
	if err := c.expect(http.MethodGet, "/api/v1/projects", "", nil, 401, "unauthorized"); err != nil {
		return err
	}
	fmt.Println("  unauthenticated request rejected with 401")

	step("create project")
	code := "SM-" + time.Now().Format("150405") + "-" + shortRandom()
	var project struct {
		ID      uuid.UUID `json:"id"`
		Code    string    `json:"code"`
		State   string    `json:"state"`
		Version int       `json:"version"`
	}
	createBody := map[string]any{
		"code":               code,
		"title":              cfg.Title,
		"total_amount_cents": 3000000,
		"deposit_cents":      900000,
		"balance_cents":      2100000,
	}
	idemKey := "smoke-create-" + code
	headers := map[string]string{"Idempotency-Key": idemKey}
	if err := c.doHeaders(http.MethodPost, "/api/v1/projects", c.token, headers, createBody, &project); err != nil {
		return err
	}
	fmt.Printf("  %s (%s) state=%s version=%d\n", project.Code, project.ID, project.State, project.Version)

	step("idempotent create")
	var replayed struct {
		ID    uuid.UUID `json:"id"`
		State string    `json:"state"`
	}
	if err := c.doHeaders(http.MethodPost, "/api/v1/projects", c.token, headers, createBody, &replayed); err != nil {
		return err
	}
	if replayed.ID != project.ID {
		return fmt.Errorf("duplicate idempotency key created a different project: %s vs %s", replayed.ID, project.ID)
	}
	if err := c.expectHeaders(http.MethodPost, "/api/v1/projects", c.token, headers,
		map[string]any{"title": "Different body"}, 409, "idempotency_conflict"); err != nil {
		return err
	}
	fmt.Println("  same key replays the project, different body -> 409 idempotency_conflict")

	step("project validation")
	if err := c.expect(http.MethodPost, "/api/v1/projects", c.token, createBody, 409, "code_conflict"); err != nil {
		return err
	}
	foreignClient := map[string]any{"title": "Foreign client", "client_id": uuid.New()}
	if err := c.expect(http.MethodPost, "/api/v1/projects", c.token, foreignClient, 422, "validation_failed"); err != nil {
		return err
	}
	badRange := map[string]any{
		"title":          "Bad range",
		"shoot_start_at": "2026-11-02T10:00:00Z",
		"shoot_end_at":   "2026-11-01T10:00:00Z",
	}
	if err := c.expect(http.MethodPost, "/api/v1/projects", c.token, badRange, 422, "validation_failed"); err != nil {
		return err
	}
	fmt.Println("  duplicate code -> 409, foreign client -> 422, inverted shoot range -> 422")

	step("state machine")
	transitions := "/api/v1/projects/" + project.ID.String() + "/transitions"
	if err := c.expect(http.MethodPost, transitions, c.token,
		map[string]any{"to": "quoted"}, 422, "validation_failed"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, transitions, c.token,
		map[string]any{"to": "quoted", "expected_version": 999}, 409, "version_conflict"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, transitions, c.token,
		map[string]any{"to": "completed", "expected_version": project.Version}, 409, "illegal_transition"); err != nil {
		return err
	}
	var quoted struct {
		State   string `json:"state"`
		Version int    `json:"version"`
	}
	if err := c.do(http.MethodPost, transitions, c.token,
		map[string]any{"to": "quoted", "expected_version": project.Version}, &quoted); err != nil {
		return err
	}
	if quoted.State != "quoted" || quoted.Version != project.Version+1 {
		return fmt.Errorf("transition result state=%s version=%d", quoted.State, quoted.Version)
	}
	var contractPending struct {
		State   string `json:"state"`
		Version int    `json:"version"`
	}
	if err := c.do(http.MethodPost, transitions, c.token,
		map[string]any{"to": "contract_pending", "expected_version": quoted.Version}, &contractPending); err != nil {
		return err
	}
	if contractPending.State != "contract_pending" {
		return fmt.Errorf("transition result state=%s", contractPending.State)
	}
	var contracted struct {
		State   string `json:"state"`
		Version int    `json:"version"`
	}
	if err := c.do(http.MethodPost, transitions, c.token,
		map[string]any{"to": "contracted", "expected_version": contractPending.Version}, &contracted); err != nil {
		return err
	}
	if contracted.State != "contracted" {
		return fmt.Errorf("transition result state=%s, want contracted", contracted.State)
	}
	fmt.Println("  missing version -> 422, stale version -> 409, illegal jump -> 409")
	fmt.Println("  draft->quoted->contract_pending->contracted -> 200 without signature")

	if err := runCommerce(c, cfg); err != nil {
		return err
	}

	if _, err := runAgent(c); err != nil {
		return err
	}

	content := make([]byte, cfg.Size)
	if _, err := rand.Read(content); err != nil {
		return err
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	fmt.Printf("  fixture %d bytes sha256=%s\n", len(content), hash[:16]+"...")

	step("open ingest batch")
	var batch struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches", c.token,
		map[string]any{
			"project_id":       project.ID,
			"device_id":        "smoke-runner",
			"card_fingerprint": "smoke-" + time.Now().Format("20060102150405"),
		}, &batch); err != nil {
		return err
	}

	step("register files")
	file := map[string]any{
		"rel_path":    "100CANON/IMG_0001.JPG",
		"size":        len(content),
		"sha256":      hash,
		"mime":        "image/jpeg",
		"width":       6000,
		"height":      4000,
		"captured_at": time.Now().Add(-time.Hour).Format(time.RFC3339),
		"camera":      "Smoke Camera",
		"exif":        map[string]any{"iso": 400},
	}
	payload := map[string]any{"files": []map[string]any{file}}
	var registered registerResult
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/assets/batch", c.token,
		payload, &registered); err != nil {
		return err
	}
	if len(registered.Assets) != 1 {
		return fmt.Errorf("expected 1 asset, got %d", len(registered.Assets))
	}
	assetID := registered.Assets[0].AssetID
	if registered.Assets[0].Status != "registered" {
		return fmt.Errorf("new asset status = %q, want registered", registered.Assets[0].Status)
	}
	fmt.Printf("  inserted=%d duplicated=%d asset=%s\n", registered.Inserted, registered.Duplicated, assetID)

	step("re-register same bytes")
	var deduped registerResult
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/assets/batch", c.token,
		payload, &deduped); err != nil {
		return err
	}
	if deduped.Inserted != 0 || deduped.Duplicated != 1 {
		return fmt.Errorf("expected dedup (inserted=0 duplicated=1), got inserted=%d duplicated=%d",
			deduped.Inserted, deduped.Duplicated)
	}
	if deduped.Assets[0].AssetID != assetID {
		return fmt.Errorf("dedup returned a different asset id %s", deduped.Assets[0].AssetID)
	}
	fmt.Println("  content hash deduplicated to the same asset")

	step("presign upload")
	plan, err := c.presign(assetID, int64(len(content)))
	if err != nil {
		return err
	}
	fmt.Printf("  mode=%s key=%s\n", plan.Mode, plan.ObjectKey)

	var parts []completePart
	switch plan.Mode {
	case "single":
		if err := c.putFile(plan.URL, content); err != nil {
			return err
		}
	case "multipart":
		reused, err := c.presign(assetID, int64(len(content)))
		if err != nil {
			return err
		}
		if reused.UploadID != plan.UploadID || reused.PartSize != plan.PartSize {
			return fmt.Errorf("re-presign opened a new upload (%s -> %s)", plan.UploadID, reused.UploadID)
		}
		fmt.Printf("  re-presign reused upload %s\n", plan.UploadID)
		parts, err = c.putParts(plan, content)
		if err != nil {
			return err
		}
		fmt.Printf("  uploaded %d parts of %d bytes\n", len(parts), plan.PartSize)
	default:
		return fmt.Errorf("unknown upload mode %q", plan.Mode)
	}

	step("complete upload")
	var asset assetMeta
	if err := c.do(http.MethodPost, "/agent/v1/uploads/"+assetID.String()+"/complete", c.token,
		map[string]any{"upload_id": plan.UploadID, "parts": parts}, &asset); err != nil {
		return err
	}
	fmt.Printf("  status=%s size=%d\n", asset.Status, asset.SizeBytes)
	if asset.Status != "uploaded" {
		return fmt.Errorf("asset status after upload = %s, want uploaded", asset.Status)
	}

	step("finalize batch")
	var finalized finalizeJob
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/finalize", c.token,
		nil, &finalized); err != nil {
		return err
	}
	if finalized.ID == "" {
		return fmt.Errorf("finalize returned no job id: %+v", finalized)
	}
	job, err := c.waitJob(finalized.ID)
	if err != nil {
		return err
	}
	if job.Result.FileCount != 1 || job.Result.Verified != 1 || job.Result.Missing != 0 || job.Result.Corrupt != 0 {
		return fmt.Errorf("verify result = %+v, want 1 file verified", job.Result)
	}
	fmt.Printf("  job=%s files=%d verified=%d missing=%d corrupt=%d\n",
		job.ID, job.Result.FileCount, job.Result.Verified, job.Result.Missing, job.Result.Corrupt)

	verifyJobID, err := uuid.Parse(finalized.ID)
	if err != nil {
		return fmt.Errorf("finalize job id %q: %w", finalized.ID, err)
	}
	if err := checkBatchStatus(c, batch.ID, verifyJobID); err != nil {
		return err
	}

	step("re-finalize (same job)")
	var refinalized finalizeJob
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/finalize", c.token,
		nil, &refinalized); err != nil {
		return err
	}
	if refinalized.ID != finalized.ID {
		return fmt.Errorf("re-finalize created a new job: %s -> %s", finalized.ID, refinalized.ID)
	}
	again, err := c.waitJob(refinalized.ID)
	if err != nil {
		return err
	}
	if again.Result != job.Result {
		return fmt.Errorf("re-finalize changed the result: %+v -> %+v", job.Result, again.Result)
	}
	fmt.Println("  same job and result returned")

	step("closed batch rejects registration")
	if err := c.expect(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/assets/batch",
		c.token, payload, 409, "batch_closed"); err != nil {
		return err
	}
	fmt.Println("  batch_closed returned")

	step("list assets")
	var listed struct {
		Total int         `json:"total"`
		Items []assetMeta `json:"items"`
	}
	if err := c.do(http.MethodGet, "/api/v1/projects/"+project.ID.String()+"/assets", c.token, nil, &listed); err != nil {
		return err
	}
	if listed.Total != 1 {
		return fmt.Errorf("expected 1 asset in project, got %d", listed.Total)
	}
	if listed.Items[0].Status != "verified" {
		return fmt.Errorf("asset status = %s, want verified", listed.Items[0].Status)
	}
	fmt.Printf("  total=%d first=%s status=%s\n", listed.Total, listed.Items[0].OriginalKey, listed.Items[0].Status)

	step("verified asset is immutable")
	if err := c.expect(http.MethodPost, "/agent/v1/uploads/presign", c.token,
		map[string]any{"asset_id": assetID, "size": len(content)}, 409, "asset_immutable"); err != nil {
		return err
	}
	fmt.Println("  re-presigning a verified asset returned 409")

	if cfg.Bulk > 0 {
		if err := runBulk(c, cfg); err != nil {
			return err
		}
	}

	if err := runGallery(c, cfg); err != nil {
		return err
	}

	if err := runProof(c, cfg); err != nil {
		return err
	}

	if err := runDelivery(c, cfg); err != nil {
		return err
	}

	fmt.Println("\nM0+M1+M2+M3+M4+M5+M6+M7 loop OK: auth -> project -> commerce/signature -> payments/refunds -> invoices/alerts -> scheduling/holds/expiry/race -> prep/call-sheet/reminders -> change orders -> agent devices/heartbeat -> ingest -> presigned upload -> finalize -> batch status -> list -> gallery publish/token/session/selection/submit/revoke -> proof rounds/tokens/annotations/revisions/gate/email/revocation -> delivery unpaid gate/build/web+original ZIPs/download links/limits/revocation/refund invalidation/archive verification/retention/deletion guard")
	return nil
}

func runBulk(c *client, cfg smokeConfig) error {
	if cfg.Bulk > 500 {
		return fmt.Errorf("bulk %d exceeds the 500 file request limit", cfg.Bulk)
	}

	step(fmt.Sprintf("bulk register %d files", cfg.Bulk))
	var project struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects", c.token,
		map[string]any{"title": cfg.Title + " bulk"}, &project); err != nil {
		return err
	}
	var batch struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches", c.token,
		map[string]any{"project_id": project.ID, "device_id": "smoke-bulk"}, &batch); err != nil {
		return err
	}

	files := make([]map[string]any, cfg.Bulk)
	for i := range files {
		sum := sha256.Sum256([]byte(fmt.Sprintf("bulk-%d-%s", i, project.ID)))
		files[i] = map[string]any{
			"rel_path": fmt.Sprintf("100CANON/BULK_%04d.CR3", i),
			"size":     24<<20 + i,
			"sha256":   hex.EncodeToString(sum[:]),
			"mime":     "image/x-canon-cr3",
		}
	}
	start := time.Now()
	var registered registerResult
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/assets/batch", c.token,
		map[string]any{"files": files}, &registered); err != nil {
		return err
	}
	elapsed := time.Since(start)
	if registered.Accepted != cfg.Bulk || registered.Inserted != cfg.Bulk || len(registered.Assets) != cfg.Bulk {
		return fmt.Errorf("bulk register = accepted %d inserted %d assets %d, want %d",
			registered.Accepted, registered.Inserted, len(registered.Assets), cfg.Bulk)
	}
	fmt.Printf("  accepted=%d inserted=%d in %s (%.0f files/s)\n",
		registered.Accepted, registered.Inserted, elapsed.Round(time.Millisecond),
		float64(cfg.Bulk)/elapsed.Seconds())

	probe := registered.Assets[0].AssetID
	step("ingest negative checks")
	if err := c.expect(http.MethodPost, "/agent/v1/uploads/"+probe.String()+"/complete", c.token,
		map[string]any{}, 409, "upload_not_started"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/agent/v1/uploads/presign", c.token,
		map[string]any{"asset_id": probe, "size": 1 << 20}, 422, "size_mismatch"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/agent/v1/uploads/presign", c.token,
		map[string]any{"asset_id": probe}, 422, "validation_failed"); err != nil {
		return err
	}
	fmt.Println("  complete before presign -> 409, wrong size -> 422, missing size -> 422")

	step("finalize bulk batch")
	var finalized finalizeJob
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/finalize", c.token,
		nil, &finalized); err != nil {
		return err
	}
	job, err := c.waitJob(finalized.ID)
	if err != nil {
		return err
	}
	if job.Result.FileCount != cfg.Bulk || job.Result.Verified != 0 || job.Result.Missing != cfg.Bulk || job.Result.Corrupt != 0 {
		return fmt.Errorf("bulk verify = %+v, want %d files missing (never uploaded)", job.Result, cfg.Bulk)
	}
	fmt.Printf("  files=%d verified=%d missing=%d\n",
		job.Result.FileCount, job.Result.Verified, job.Result.Missing)
	return nil
}

func (c *client) presign(assetID uuid.UUID, size int64) (*uploadPlan, error) {
	var plan uploadPlan
	if err := c.do(http.MethodPost, "/agent/v1/uploads/presign", c.token,
		map[string]any{"asset_id": assetID, "size": size}, &plan); err != nil {
		return nil, err
	}
	if plan.Mode == "" || plan.ObjectKey == "" {
		return nil, fmt.Errorf("presign returned an empty plan: %+v", plan)
	}
	return &plan, nil
}

func (c *client) do(method, path, token string, body any, out any) error {
	return c.doHeaders(method, path, token, nil, body, out)
}

func (c *client) doHeaders(method, path, token string, headers map[string]string, body any, out any) error {
	status, raw, err := c.doStatusHeaders(method, path, token, headers, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s %s: %d: %s", method, path, status, string(raw))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (c *client) expect(method, path, token string, body any, wantStatus int, wantCode string) error {
	return c.expectHeaders(method, path, token, nil, body, wantStatus, wantCode)
}

func (c *client) expectHeaders(method, path, token string, headers map[string]string, body any, wantStatus int, wantCode string) error {
	status, raw, err := c.doStatusHeaders(method, path, token, headers, body)
	if err != nil {
		return err
	}
	if status != wantStatus {
		return fmt.Errorf("%s %s: status %d, want %d: %s", method, path, status, wantStatus, string(raw))
	}
	if wantCode != "" {
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.Code != wantCode {
			return fmt.Errorf("%s %s: code %q, want %q: %s", method, path, p.Code, wantCode, string(raw))
		}
	}
	return nil
}

func (c *client) doText(path string, out *string) error {
	status, raw, err := c.doStatus(http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: %d: %s", path, status, string(raw))
	}
	*out = string(raw)
	return nil
}

func (c *client) waitJob(id string) (*finalizeJob, error) {
	deadline := time.Now().Add(c.jobTimeout)
	for {
		var job finalizeJob
		if err := c.do(http.MethodGet, "/agent/v1/jobs/"+id, c.token, nil, &job); err != nil {
			return nil, err
		}
		switch job.Status {
		case "succeeded":
			return &job, nil
		case "failed":
			return nil, fmt.Errorf("job %s failed: %s", id, job.LastError)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("job %s still %s after %s (is the worker running?)", id, job.Status, c.jobTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (c *client) doStatus(method, path, token string, body any) (int, []byte, error) {
	return c.doStatusHeaders(method, path, token, nil, body)
}

func (c *client) doStatusHeaders(method, path, token string, headers map[string]string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

func (c *client) putFile(url string, content []byte) error {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(content))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload PUT: %s: %s", resp.Status, string(raw))
	}
	return nil
}

func (c *client) putParts(plan *uploadPlan, content []byte) ([]completePart, error) {
	parts := make([]completePart, 0, len(plan.Parts))
	for _, part := range plan.Parts {
		start := int64(part.PartNumber-1) * plan.PartSize
		end := start + plan.PartSize
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		req, err := http.NewRequest(http.MethodPut, part.URL, bytes.NewReader(content[start:end]))
		if err != nil {
			return nil, err
		}
		req.ContentLength = end - start
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("part %d: %s: %s", part.PartNumber, resp.Status, string(raw))
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return nil, fmt.Errorf("part %d: missing ETag", part.PartNumber)
		}
		parts = append(parts, completePart{PartNumber: part.PartNumber, ETag: etag})
	}
	return parts, nil
}

func shortRandom() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

func step(name string) {
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), name)
}
