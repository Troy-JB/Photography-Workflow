package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/payment"
)

type deliveryPackageView struct {
	ID         uuid.UUID  `json:"id"`
	Spec       string     `json:"spec"`
	Version    int        `json:"version"`
	Status     string     `json:"status"`
	FileName   string     `json:"file_name"`
	SizeBytes  int64      `json:"size_bytes"`
	SHA256     string     `json:"sha256"`
	EntryCount int        `json:"entry_count"`
	BuiltAt    *time.Time `json:"built_at"`
}

type deliveryTokenView struct {
	ID           uuid.UUID  `json:"id"`
	Label        string     `json:"label"`
	MaxDownloads int        `json:"max_downloads"`
	Downloads    int        `json:"downloads"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at"`
}

type issuedDeliveryToken struct {
	deliveryTokenView
	Plaintext string `json:"token"`
	URL       string `json:"url"`
}

type deliveryDetailView struct {
	ID            uuid.UUID             `json:"id"`
	ProjectID     uuid.UUID             `json:"project_id"`
	Status        string                `json:"status"`
	BuildVersion  int                   `json:"build_version"`
	ItemCount     int                   `json:"item_count"`
	TotalBytes    int64                 `json:"total_bytes"`
	ReadyAt       *time.Time            `json:"ready_at"`
	BlockedReason string                `json:"blocked_reason"`
	LastError     string                `json:"last_error"`
	PaymentStatus string                `json:"payment_status"`
	ProjectState  string                `json:"project_state"`
	WebReady      int                   `json:"web_ready"`
	WebSkipped    int                   `json:"web_skipped"`
	Packages      []deliveryPackageView `json:"packages"`
	Tokens        []deliveryTokenView   `json:"tokens"`
}

type deliveryItemView struct {
	ID             uuid.UUID `json:"id"`
	AssetID        uuid.UUID `json:"asset_id"`
	Position       int       `json:"position"`
	OriginalName   string    `json:"original_name"`
	OriginalSize   int64     `json:"original_size"`
	OriginalSHA256 string    `json:"original_sha256"`
	RevisionNo     int       `json:"revision_no"`
	WebSize        int64     `json:"web_size"`
	WebSHA256      string    `json:"web_sha256"`
	WebWidth       int       `json:"web_width"`
	WebHeight      int       `json:"web_height"`
	WebStatus      string    `json:"web_status"`
}

type customerDeliveryPackage struct {
	ID         uuid.UUID `json:"id"`
	Spec       string    `json:"spec"`
	FileName   string    `json:"file_name"`
	SizeBytes  int64     `json:"size_bytes"`
	EntryCount int       `json:"entry_count"`
}

type customerDelivery struct {
	ID                 uuid.UUID                 `json:"id"`
	ProjectTitle       string                    `json:"project_title"`
	RoundNo            int                       `json:"round_no"`
	ItemCount          int                       `json:"item_count"`
	DownloadsRemaining int                       `json:"downloads_remaining"`
	ExpiresAt          time.Time                 `json:"expires_at"`
	WebReady           int                       `json:"web_ready"`
	WebSkipped         int                       `json:"web_skipped"`
	Packages           []customerDeliveryPackage `json:"packages"`
}

type deliverySessionView struct {
	Session   string           `json:"session"`
	ExpiresAt time.Time        `json:"expires_at"`
	Delivery  customerDelivery `json:"delivery"`
}

type customerDeliveryItem struct {
	ID           uuid.UUID `json:"id"`
	Position     int       `json:"position"`
	FileName     string    `json:"file_name"`
	OriginalSize int64     `json:"original_size"`
	RevisionNo   int       `json:"revision_no"`
	WebAvailable bool      `json:"web_available"`
	WebWidth     int       `json:"web_width"`
	WebHeight    int       `json:"web_height"`
	WebSize      int64     `json:"web_size"`
	ThumbURL     string    `json:"thumb_url"`
}

type deliveryItemsPage struct {
	Items  []customerDeliveryItem `json:"items"`
	Total  int                    `json:"total"`
	Limit  int                    `json:"limit"`
	Offset int                    `json:"offset"`
}

type deliveryGrantView struct {
	URL                string    `json:"url"`
	FileName           string    `json:"file_name"`
	ExpiresAt          time.Time `json:"expires_at"`
	DownloadsRemaining int       `json:"downloads_remaining"`
}

type deliveryManifestView struct {
	SchemaVersion int       `json:"schema_version"`
	DeliveryID    uuid.UUID `json:"delivery_id"`
	ProjectCode   string    `json:"project_code"`
	Spec          string    `json:"spec"`
	BuildVersion  int       `json:"build_version"`
	Items         []struct {
		Entry        string    `json:"entry"`
		AssetID      uuid.UUID `json:"asset_id"`
		OriginalName string    `json:"original_name"`
		Source       string    `json:"source"`
		RevisionNo   int       `json:"revision_no"`
		SHA256       string    `json:"sha256"`
		Size         int64     `json:"size"`
	} `json:"items"`
	Warnings []string `json:"warnings"`
}

type archiveView struct {
	ID             uuid.UUID  `json:"id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	Status         string     `json:"status"`
	Version        int        `json:"version"`
	ObjectCount    int        `json:"object_count"`
	VerifiedCount  int        `json:"verified_count"`
	FailedCount    int        `json:"failed_count"`
	TotalBytes     int64      `json:"total_bytes"`
	KeepDays       int        `json:"keep_days"`
	WarnDays       int        `json:"warn_days"`
	RetentionUntil *time.Time `json:"retention_until"`
	VerifiedAt     *time.Time `json:"verified_at"`
	LastError      string     `json:"last_error"`
}

type retentionPolicyView struct {
	KeepDays int `json:"keep_days"`
	WarnDays int `json:"warn_days"`
}

// runDelivery exercises M7: the strict balance gate, the two-spec build, the
// customer download link (session, grants, limits, revocation), refund
// invalidation and the archive verification/retention snapshot.
func runDelivery(c *client, cfg smokeConfig) error {
	tag := time.Now().Format("150405") + "-" + shortRandom()
	clientEmail := fmt.Sprintf("delivery+%s@example.com", strings.ToLower(shortRandom()))

	step("delivery: contracted project (unpaid) with photos and an approved round")
	projectID, err := c.contractSignProject(cfg, tag, clientEmail)
	if err != nil {
		return err
	}

	contents := make([][]byte, 2)
	paths := make([]string, len(contents))
	files := make([]map[string]any, len(contents))
	for i := range contents {
		body, err := galleryFixture(900, 650, 0.4+float64(i)*0.3)
		if err != nil {
			return fmt.Errorf("encode delivery fixture %d: %w", i, err)
		}
		contents[i] = body
		paths[i] = fmt.Sprintf("delivery/DLV_%02d.JPG", i+1)
		files[i] = map[string]any{
			"rel_path": paths[i],
			"size":     len(body),
			"sha256":   shaHex(body),
			"mime":     "image/jpeg",
			"width":    900,
			"height":   650,
		}
	}
	assetIDs, err := c.ingestFixtures(projectID, "smoke-delivery", contents, files)
	if err != nil {
		return err
	}
	roundID, err := c.approvedRound(projectID, assetIDs)
	if err != nil {
		return err
	}
	fmt.Printf("  project %s, round %s approved, client %s\n", projectID, roundID, clientEmail)

	step("delivery: create while unpaid")
	status, raw, err := c.doStatus(http.MethodPost, "/api/v1/deliveries", c.token,
		map[string]any{"proof_round_id": roundID})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("create delivery = %d: %s", status, string(raw))
	}
	var created deliveryDetailView
	if err := json.Unmarshal(raw, &created); err != nil {
		return fmt.Errorf("decode delivery: %w", err)
	}
	deliveryID := created.ID
	if created.Status != "pending" || created.PaymentStatus != "unpaid" {
		return fmt.Errorf("delivery after create = status %s payment %s", created.Status, created.PaymentStatus)
	}
	status, raw, err = c.doStatus(http.MethodPost, "/api/v1/deliveries", c.token,
		map[string]any{"proof_round_id": roundID})
	if err != nil {
		return err
	}
	var again deliveryDetailView
	if status != http.StatusOK {
		return fmt.Errorf("second create delivery = %d: %s", status, string(raw))
	}
	if err := json.Unmarshal(raw, &again); err != nil {
		return fmt.Errorf("decode duplicate delivery: %w", err)
	}
	if again.ID != deliveryID {
		return fmt.Errorf("duplicate create made a new delivery: %s -> %s", deliveryID, again.ID)
	}
	if err := c.expect(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/build",
		c.token, nil, http.StatusConflict, "payment_required"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/tokens",
		c.token, map[string]any{"label": "unpaid"}, http.StatusConflict, "payment_required"); err != nil {
		return err
	}
	fmt.Println("  pending delivery, duplicate create returns the same row, unpaid build/token -> 409 payment_required")

	step("delivery: deposit paid is still not enough")
	if _, err := c.payProject(cfg, projectID, "deposit", 100000, tag); err != nil {
		return err
	}
	project, err := c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "deposit_paid" {
		return fmt.Errorf("project payment_status after deposit = %s", project.PaymentStatus)
	}
	if err := c.expect(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/build",
		c.token, nil, http.StatusConflict, "payment_required"); err != nil {
		return err
	}
	fmt.Println("  deposit paid, build still 409 payment_required")

	step("delivery: balance paid, build both specs")
	if _, err := c.payProject(cfg, projectID, "balance", 200000, tag); err != nil {
		return err
	}
	var building deliveryDetailView
	if err := c.do(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/build", c.token, nil, &building); err != nil {
		return err
	}
	if building.Status != "building" || building.BuildVersion != 1 {
		return fmt.Errorf("build accepted with status %s version %d, want building v1", building.Status, building.BuildVersion)
	}
	var racing deliveryDetailView
	if err := c.do(http.MethodGet, "/api/v1/deliveries/"+deliveryID.String(), c.token, nil, &racing); err != nil {
		return err
	}
	if racing.Status == "building" {
		if err := c.expect(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/build",
			c.token, nil, http.StatusConflict, "build_in_progress"); err != nil {
			return err
		}
		fmt.Println("  paid build accepted, concurrent rebuild -> 409 build_in_progress")
	} else {
		fmt.Printf("  paid build accepted (build already %s; in-progress guard covered by unit tests)\n", racing.Status)
	}
	detail, err := c.waitDeliveryReady(deliveryID)
	if err != nil {
		return err
	}
	if detail.ItemCount != len(assetIDs) || detail.WebReady != len(assetIDs) || detail.WebSkipped != 0 {
		return fmt.Errorf("delivery ready = items %d web %d skipped %d, want %d",
			detail.ItemCount, detail.WebReady, detail.WebSkipped, len(assetIDs))
	}
	if detail.ReadyAt == nil || detail.TotalBytes <= 0 {
		return fmt.Errorf("ready delivery lacks ready_at/total_bytes: %+v", detail)
	}
	pkgs := map[string]deliveryPackageView{}
	for _, pkg := range detail.Packages {
		if pkg.Version != detail.BuildVersion {
			continue
		}
		pkgs[pkg.Spec] = pkg
	}
	for _, spec := range []string{"web", "original"} {
		pkg, ok := pkgs[spec]
		if !ok {
			return fmt.Errorf("delivery is ready without a %s package", spec)
		}
		if pkg.Status != "ready" || pkg.SizeBytes <= 0 || len(pkg.SHA256) != 64 ||
			pkg.EntryCount != len(assetIDs) || !strings.HasSuffix(pkg.FileName, ".zip") {
			return fmt.Errorf("%s package = %+v", spec, pkg)
		}
	}
	fmt.Printf("  ready v%d: %d items / %d bytes, web + original ZIPs verified\n",
		detail.BuildVersion, detail.ItemCount, detail.TotalBytes)

	step("delivery: issue link and download both packages")
	var tokenA issuedDeliveryToken
	if err := c.do(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/tokens", c.token,
		map[string]any{"label": "smoke package", "expires_in_days": 1, "max_downloads": 2}, &tokenA); err != nil {
		return err
	}
	if len(tokenA.Plaintext) < 32 || !strings.Contains(tokenA.URL, "/#/d/"+tokenA.Plaintext) {
		return fmt.Errorf("delivery link %q is not a fragment link", tokenA.URL)
	}
	var listedTokens struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := c.do(http.MethodGet, "/api/v1/deliveries/"+deliveryID.String()+"/tokens", c.token, nil, &listedTokens); err != nil {
		return err
	}
	if len(listedTokens.Items) == 0 {
		return fmt.Errorf("token list is empty after issuing one")
	}
	for _, item := range listedTokens.Items {
		for _, key := range []string{"token", "plaintext", "token_hash"} {
			if _, ok := item[key]; ok {
				return fmt.Errorf("token list leaked %q", key)
			}
		}
	}

	sessionA, err := c.deliverySession(tokenA.Plaintext)
	if err != nil {
		return err
	}
	if sessionA.Session == "" || sessionA.Session == tokenA.Plaintext {
		return fmt.Errorf("delivery session must be a separate credential")
	}
	if sessionA.Delivery.ID != deliveryID || len(sessionA.Delivery.Packages) != 2 {
		return fmt.Errorf("delivery session = %+v", sessionA.Delivery)
	}
	if sessionA.Delivery.DownloadsRemaining != 2 || sessionA.Delivery.WebReady != len(assetIDs) {
		return fmt.Errorf("session remaining %d web %d", sessionA.Delivery.DownloadsRemaining, sessionA.Delivery.WebReady)
	}
	status, headers, raw, err := c.galleryCall(http.MethodGet, "/g/v1/delivery/session", sessionA.Session, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("delivery summary: %d: %s", status, string(raw))
	}
	if headers.Get("Cache-Control") != "no-store" || headers.Get("Referrer-Policy") != "no-referrer" {
		return fmt.Errorf("delivery headers cache=%q referrer=%q",
			headers.Get("Cache-Control"), headers.Get("Referrer-Policy"))
	}

	for _, spec := range []string{"web", "original"} {
		pkg := pkgs[spec]
		grant, err := c.download(sessionA.Session, map[string]any{"package_id": pkg.ID})
		if err != nil {
			return err
		}
		if grant.FileName != pkg.FileName {
			return fmt.Errorf("%s package grant name = %q, want %q", spec, grant.FileName, pkg.FileName)
		}
		body, err := c.fetchGrant(grant.URL, pkg.SizeBytes)
		if err != nil {
			return fmt.Errorf("download %s package: %w", spec, err)
		}
		if got := shaHex(body); got != pkg.SHA256 {
			return fmt.Errorf("%s package sha = %s, want %s", spec, got, pkg.SHA256)
		}
		manifest, err := zipManifest(body)
		if err != nil {
			return fmt.Errorf("read %s manifest: %w", spec, err)
		}
		if manifest.SchemaVersion < 1 || manifest.DeliveryID != deliveryID || manifest.Spec != spec ||
			manifest.BuildVersion != detail.BuildVersion || len(manifest.Items) != len(assetIDs) {
			return fmt.Errorf("%s manifest = %+v", spec, manifest)
		}
		for _, item := range manifest.Items {
			if item.Entry == "" || item.OriginalName == "" || len(item.SHA256) != 64 || item.Size <= 0 {
				return fmt.Errorf("%s manifest item = %+v", spec, item)
			}
		}
	}
	if err := c.expect(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/tokens/"+
		tokenA.ID.String()+"/revoke", c.token, nil, http.StatusNoContent, ""); err != nil {
		return err
	}
	if err := c.expect(http.MethodGet, "/g/v1/delivery/session", sessionA.Session, nil,
		http.StatusGone, "link_revoked"); err != nil {
		return err
	}
	fmt.Println("  both ZIPs match their SHA-256 and manifest, revoke kills the session with 410 link_revoked")

	step("delivery: per-item downloads and the download limit")
	var tokenB issuedDeliveryToken
	if err := c.do(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/tokens", c.token,
		map[string]any{"label": "smoke items", "max_downloads": 2}, &tokenB); err != nil {
		return err
	}
	sessionB, err := c.deliverySession(tokenB.Plaintext)
	if err != nil {
		return err
	}
	var itemsPage deliveryItemsPage
	if err := c.do(http.MethodGet, "/g/v1/delivery/items?limit=60", sessionB.Session, nil, &itemsPage); err != nil {
		return err
	}
	if itemsPage.Total != len(assetIDs) || len(itemsPage.Items) != len(assetIDs) {
		return fmt.Errorf("customer items = total %d items %d", itemsPage.Total, len(itemsPage.Items))
	}
	var staffItems struct {
		Items []deliveryItemView `json:"items"`
		Total int                `json:"total"`
	}
	if err := c.do(http.MethodGet, "/api/v1/deliveries/"+deliveryID.String()+"/items", c.token, nil, &staffItems); err != nil {
		return err
	}
	byID := map[uuid.UUID]deliveryItemView{}
	for _, item := range staffItems.Items {
		byID[item.ID] = item
	}
	var webItem customerDeliveryItem
	for _, item := range itemsPage.Items {
		if item.WebAvailable {
			webItem = item
			break
		}
	}
	if webItem.ID == uuid.Nil {
		return fmt.Errorf("no delivered item has a web version")
	}
	webSource, ok := byID[webItem.ID]
	if !ok || webSource.WebStatus != "ready" || webSource.WebSHA256 == "" {
		return fmt.Errorf("staff item view lacks the web render for %s", webItem.ID)
	}

	original, err := c.download(sessionB.Session, map[string]any{"item_id": webSource.ID, "spec": "original"})
	if err != nil {
		return err
	}
	body, err := c.fetchGrant(original.URL, webSource.OriginalSize)
	if err != nil {
		return fmt.Errorf("download original item: %w", err)
	}
	if got := shaHex(body); got != webSource.OriginalSHA256 {
		return fmt.Errorf("original item sha = %s, want %s", got, webSource.OriginalSHA256)
	}
	if body[0] != 0xFF || body[1] != 0xD8 {
		return fmt.Errorf("original item does not start with a JPEG signature")
	}

	web, err := c.download(sessionB.Session, map[string]any{"item_id": webSource.ID, "spec": "web"})
	if err != nil {
		return err
	}
	webBody, err := c.fetchGrant(web.URL, webSource.WebSize)
	if err != nil {
		return fmt.Errorf("download web item: %w", err)
	}
	if got := shaHex(webBody); got != webSource.WebSHA256 {
		return fmt.Errorf("web item sha = %s, want %s", got, webSource.WebSHA256)
	}
	if webBody[0] != 0xFF || webBody[1] != 0xD8 {
		return fmt.Errorf("web item does not start with a JPEG signature")
	}
	if err := c.expect(http.MethodPost, "/g/v1/delivery/downloads", sessionB.Session,
		map[string]any{"item_id": webSource.ID, "spec": "web"}, http.StatusConflict, "downloads_exhausted"); err != nil {
		return err
	}
	var exhausted customerDelivery
	if err := c.do(http.MethodGet, "/g/v1/delivery/session", sessionB.Session, nil, &exhausted); err != nil {
		return err
	}
	if exhausted.DownloadsRemaining != 0 {
		return fmt.Errorf("downloads remaining = %d after the limit", exhausted.DownloadsRemaining)
	}
	fmt.Println("  original bytes and web render match their hashes, 3rd grant -> 409 downloads_exhausted")

	step("delivery: refund closes the gate immediately")
	payments, err := c.listPayments(projectID)
	if err != nil {
		return err
	}
	var balancePayment *paymentRecord
	for i := range payments.Items {
		if payments.Items[i].AmountCents == 200000 {
			balancePayment = &payments.Items[i]
			break
		}
	}
	if balancePayment == nil {
		return fmt.Errorf("balance payment not found in %+v", payments.Items)
	}
	refund, err := c.createRefund(balancePayment.ID, 200000, "delivery smoke refund")
	if err != nil {
		return err
	}
	refundEvent := map[string]any{
		"event_id": "smoke-delivery-refund-" + tag, "type": payment.EventRefundSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_refund_no": refund.OutRefundNo,
		"provider_refund_id": refund.ProviderRefundID, "amount_cents": 200000, "currency": "CNY",
	}
	if err := c.expectPaymentCallback(refundEvent, cfg.PaymentSecret, 200, "processed"); err != nil {
		return err
	}
	project, err = c.getProject(projectID)
	if err != nil {
		return err
	}
	if project.PaymentStatus != "deposit_paid" {
		return fmt.Errorf("payment_status after refund = %s, want deposit_paid", project.PaymentStatus)
	}
	if err := c.expect(http.MethodGet, "/g/v1/delivery/session", sessionB.Session, nil,
		http.StatusConflict, "payment_required"); err != nil {
		return err
	}
	if err := c.expect(http.MethodGet, "/g/v1/delivery/items", sessionB.Session, nil,
		http.StatusConflict, "payment_required"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/build",
		c.token, nil, http.StatusConflict, "payment_required"); err != nil {
		return err
	}
	if _, err := c.payProject(cfg, projectID, "balance", 200000, tag+"-re"); err != nil {
		return err
	}
	if err := c.do(http.MethodGet, "/g/v1/delivery/session", sessionB.Session, nil, &exhausted); err != nil {
		return err
	}
	fmt.Println("  refund -> every request 409 payment_required; paying the balance restores the same session")

	if cfg.MailDir != "" {
		step("delivery: customer email carries the fragment link")
		var notified struct {
			MessageID uuid.UUID `json:"message_id"`
			TokenID   uuid.UUID `json:"token_id"`
		}
		if err := c.do(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/notify", c.token,
			map[string]any{"recipient": clientEmail}, &notified); err != nil {
			return err
		}
		if notified.MessageID == uuid.Nil || notified.TokenID == uuid.Nil {
			return fmt.Errorf("notify returned %+v", notified)
		}
		if err := waitCapturedProofMail(cfg.MailDir, "/#/d/", c.jobTimeout); err != nil {
			return err
		}
		fmt.Println("  delivery mail captured with a /#/d/ link")
	}

	step("delivery: archive verification and retention")
	if err := c.transitionToArchived(projectID); err != nil {
		return err
	}
	status, raw, err = c.doStatus(http.MethodPost, "/api/v1/archives", c.token,
		map[string]any{"project_id": projectID})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("create archive = %d: %s", status, string(raw))
	}
	var archive archiveView
	if err := json.Unmarshal(raw, &archive); err != nil {
		return fmt.Errorf("decode archive: %w", err)
	}
	status, raw, err = c.doStatus(http.MethodPost, "/api/v1/archives", c.token,
		map[string]any{"project_id": projectID})
	if err != nil {
		return err
	}
	var duplicate archiveView
	if status != http.StatusOK {
		return fmt.Errorf("second archive create = %d: %s", status, string(raw))
	}
	if err := json.Unmarshal(raw, &duplicate); err != nil {
		return fmt.Errorf("decode duplicate archive: %w", err)
	}
	if duplicate.ID != archive.ID {
		return fmt.Errorf("duplicate archive create made a new row: %s -> %s", archive.ID, duplicate.ID)
	}
	archive, err = c.waitArchiveReady(archive.ID)
	if err != nil {
		return err
	}
	if archive.Status != "ready" || archive.ObjectCount <= 0 ||
		archive.VerifiedCount != archive.ObjectCount || archive.FailedCount != 0 ||
		archive.TotalBytes <= 0 || archive.RetentionUntil == nil || archive.VerifiedAt == nil {
		return fmt.Errorf("archive ready = %+v", archive)
	}
	if archive.KeepDays <= 0 || archive.WarnDays < 0 || archive.WarnDays >= archive.KeepDays {
		return fmt.Errorf("archive retention = keep %d warn %d", archive.KeepDays, archive.WarnDays)
	}
	days := time.Until(*archive.RetentionUntil).Hours() / 24
	if days < float64(archive.KeepDays)-1 || days > float64(archive.KeepDays)+1 {
		return fmt.Errorf("retention_until is %.1f days away, want ~%d", days, archive.KeepDays)
	}
	var policy retentionPolicyView
	if err := c.do(http.MethodGet, "/api/v1/retention-policy", c.token, nil, &policy); err != nil {
		return err
	}
	if policy.KeepDays != archive.KeepDays || policy.WarnDays != archive.WarnDays {
		return fmt.Errorf("policy %+v does not match the archive snapshot %d/%d",
			policy, archive.KeepDays, archive.WarnDays)
	}
	var archiveItems struct {
		Items []struct {
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := c.do(http.MethodGet, "/api/v1/archives/"+archive.ID.String()+"/items?limit=500",
		c.token, nil, &archiveItems); err != nil {
		return err
	}
	if archiveItems.Total != archive.ObjectCount || len(archiveItems.Items) != archive.ObjectCount {
		return fmt.Errorf("archive items = total %d items %d, want %d",
			archiveItems.Total, len(archiveItems.Items), archive.ObjectCount)
	}
	for _, item := range archiveItems.Items {
		if item.Status != "verified" || item.Kind == "" {
			return fmt.Errorf("archive item = %+v", item)
		}
	}
	fmt.Printf("  archive %s ready: %d/%d objects verified, retention %d days (warn %d)\n",
		archive.ID, archive.VerifiedCount, archive.ObjectCount, archive.KeepDays, archive.WarnDays)

	step("delivery: archived project closes links and freezes builds")
	if err := c.expect(http.MethodPost, "/g/v1/delivery/session", "",
		map[string]any{"token": tokenB.Plaintext}, http.StatusGone, "delivery_archived"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/deliveries/"+deliveryID.String()+"/build",
		c.token, nil, http.StatusGone, "delivery_archived"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/archives/"+archive.ID.String()+"/deletion", c.token,
		map[string]any{"confirm": "", "reason": ""}, http.StatusUnprocessableEntity, "validation_failed"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/api/v1/archives/"+archive.ID.String()+"/deletion", c.token,
		map[string]any{"confirm": "DELETE", "reason": ""}, http.StatusUnprocessableEntity, "validation_failed"); err != nil {
		return err
	}
	if cfg.StaffEmail != "" {
		var staffSession struct {
			AccessToken string `json:"access_token"`
		}
		if err := c.do(http.MethodPost, "/api/v1/auth/login", "",
			map[string]string{"email": cfg.StaffEmail, "password": cfg.StaffPassword}, &staffSession); err != nil {
			return fmt.Errorf("staff login: %w", err)
		}
		if err := c.expect(http.MethodPost, "/api/v1/archives/"+archive.ID.String()+"/deletion",
			staffSession.AccessToken, map[string]any{"confirm": "DELETE", "reason": "staff cannot delete"},
			http.StatusForbidden, "forbidden"); err != nil {
			return err
		}
	}
	fmt.Println("  archived links answer 410, builds are frozen, deletion needs DELETE + reason and the owner role")

	if cfg.DatabaseURL != "" {
		if err := c.verifyDeliveryEmail(cfg, deliveryID); err != nil {
			return err
		}
	}
	return nil
}

// contractSignProject creates a client, project, quote and a signed contract
// without collecting any money, so the delivery gate can be exercised first.
func (c *client) contractSignProject(cfg smokeConfig, tag, email string) (uuid.UUID, error) {
	var cl struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/clients", c.token,
		map[string]any{"name": "Delivery Client " + tag, "email": email}, &cl); err != nil {
		return uuid.Nil, err
	}
	var project struct {
		ID      uuid.UUID `json:"id"`
		Version int       `json:"version"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects", c.token, map[string]any{
		"title": "Delivery smoke " + tag, "client_id": cl.ID,
		"total_amount_cents": 300000, "deposit_cents": 100000, "balance_cents": 200000,
	}, &project); err != nil {
		return uuid.Nil, err
	}

	var quote commerceQuote
	if err := c.do(http.MethodPost, "/api/v1/quotes", c.token, map[string]any{
		"project_id": project.ID, "deposit_cents": 100000,
		"items": []map[string]any{
			{"kind": "service", "description": "交付冒烟拍摄", "quantity": 1, "unit_price_cents": 300000},
		},
	}, &quote); err != nil {
		return uuid.Nil, err
	}
	var sent commerceQuote
	if err := c.do(http.MethodPost, "/api/v1/quotes/"+quote.ID.String()+"/send", c.token,
		map[string]any{"expected_version": quote.RowVersion}, &sent); err != nil {
		return uuid.Nil, err
	}
	var contract commerceContract
	if err := c.do(http.MethodPost, "/api/v1/contracts", c.token, map[string]any{
		"project_id": project.ID, "quote_id": sent.ID, "signer_name": "Delivery Client " + tag,
	}, &contract); err != nil {
		return uuid.Nil, err
	}
	var signing commerceContract
	if err := c.do(http.MethodPost, "/api/v1/contracts/"+contract.ID.String()+"/send", c.token,
		map[string]any{"expected_version": contract.Version}, &signing); err != nil {
		return uuid.Nil, err
	}
	for _, event := range []map[string]any{
		{"event_id": "delivery-signing-" + tag, "sign_task_id": signing.SignTaskID, "type": "signing"},
		{
			"event_id": "delivery-signed-" + tag, "sign_task_id": signing.SignTaskID, "type": "signed",
			"signer_name": "Delivery Client " + tag, "document_sha256": strings.Repeat("d", 64),
			"signed_at": time.Now().UTC().Format(time.RFC3339),
		},
	} {
		if err := c.expectCallback(event, cfg.ESignSecret, 200, "processed"); err != nil {
			return uuid.Nil, err
		}
	}
	var signed commerceContract
	if err := c.do(http.MethodGet, "/api/v1/contracts/"+contract.ID.String(), c.token, nil, &signed); err != nil {
		return uuid.Nil, err
	}
	if signed.Status != "signed" {
		return uuid.Nil, fmt.Errorf("contract status = %s, want signed", signed.Status)
	}
	return project.ID, nil
}

// ingestFixtures uploads the prepared JPEGs through the agent endpoints and
// waits for the batch verification job.
func (c *client) ingestFixtures(projectID uuid.UUID, device string, contents [][]byte, files []map[string]any) ([]uuid.UUID, error) {
	var batch struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches", c.token,
		map[string]any{"project_id": projectID, "device_id": device}, &batch); err != nil {
		return nil, err
	}
	var registered registerResult
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/assets/batch", c.token,
		map[string]any{"files": files}, &registered); err != nil {
		return nil, err
	}
	if registered.Accepted != len(files) || len(registered.Assets) != len(files) {
		return nil, fmt.Errorf("register fixtures = accepted %d assets %d, want %d",
			registered.Accepted, len(registered.Assets), len(files))
	}
	ids := make([]uuid.UUID, len(files))
	for i, entry := range registered.Assets {
		ids[i] = entry.AssetID
		plan, err := c.presign(entry.AssetID, int64(len(contents[i])))
		if err != nil {
			return nil, err
		}
		if plan.Mode != "single" {
			return nil, fmt.Errorf("fixture used %s upload mode, want single", plan.Mode)
		}
		if err := c.putFile(plan.URL, contents[i]); err != nil {
			return nil, err
		}
		var asset assetMeta
		if err := c.do(http.MethodPost, "/agent/v1/uploads/"+entry.AssetID.String()+"/complete", c.token,
			map[string]any{"upload_id": plan.UploadID, "parts": []completePart{}}, &asset); err != nil {
			return nil, err
		}
		if asset.Status != "uploaded" {
			return nil, fmt.Errorf("fixture asset %d status = %s, want uploaded", i, asset.Status)
		}
	}
	var finalized finalizeJob
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/finalize", c.token,
		nil, &finalized); err != nil {
		return nil, err
	}
	job, err := c.waitJob(finalized.ID)
	if err != nil {
		return nil, err
	}
	if job.Result.Verified != len(files) || job.Result.Missing != 0 || job.Result.Corrupt != 0 {
		return nil, fmt.Errorf("fixture verify result = %+v, want %d verified", job.Result, len(files))
	}
	return ids, nil
}

// approvedRound publishes a gallery, submits the customer selection and drives
// a proof round to approved so a delivery can be created.
func (c *client) approvedRound(projectID uuid.UUID, assetIDs []uuid.UUID) (uuid.UUID, error) {
	var gallery galleryView
	if err := c.do(http.MethodPost, "/api/v1/galleries", c.token, map[string]any{
		"project_id":      projectID,
		"title":           "Delivery smoke " + shortRandom(),
		"expires_at":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"selection_limit": len(assetIDs),
	}, &gallery); err != nil {
		return uuid.Nil, err
	}
	galleryPath := "/api/v1/galleries/" + gallery.ID.String()
	var attached struct {
		Added int `json:"added"`
	}
	if err := c.do(http.MethodPost, galleryPath+"/assets", c.token,
		map[string]any{"asset_ids": assetIDs}, &attached); err != nil {
		return uuid.Nil, err
	}
	if attached.Added != len(assetIDs) {
		return uuid.Nil, fmt.Errorf("gallery attach = %d, want %d", attached.Added, len(assetIDs))
	}
	if err := c.do(http.MethodPost, "/api/v1/projects/"+projectID.String()+"/derivatives:derive", c.token,
		map[string]any{"asset_ids": []uuid.UUID{}, "force": false}, &struct {
			Candidates int `json:"candidates"`
		}{}); err != nil {
		return uuid.Nil, err
	}
	if err := c.waitDerivatives(projectID, assetIDs); err != nil {
		return uuid.Nil, err
	}
	if err := c.do(http.MethodPost, galleryPath+"/publish", c.token, nil, &gallery); err != nil {
		return uuid.Nil, err
	}
	if gallery.Status != "published" {
		return uuid.Nil, fmt.Errorf("gallery status = %s, want published", gallery.Status)
	}

	var share issuedShare
	if err := c.do(http.MethodPost, galleryPath+"/tokens", c.token,
		map[string]any{"label": "delivery smoke"}, &share); err != nil {
		return uuid.Nil, err
	}
	var selectionSession gallerySession
	if err := c.do(http.MethodPost, "/g/v1/session", "",
		map[string]any{"token": share.Token}, &selectionSession); err != nil {
		return uuid.Nil, err
	}
	items := make([]map[string]any, len(assetIDs))
	for i, id := range assetIDs {
		items[i] = map[string]any{"asset_id": id, "decision": "selected"}
	}
	var selection selectionResult
	if err := c.do(http.MethodPost, "/g/v1/selections", selectionSession.Session, map[string]any{
		"expected_version": 0, "items": items,
	}, &selection); err != nil {
		return uuid.Nil, err
	}
	if selection.Counts.Selected != len(assetIDs) {
		return uuid.Nil, fmt.Errorf("selection counts = %+v", selection.Counts)
	}
	var submitted galleryAlbum
	if err := c.do(http.MethodPost, "/g/v1/albums/"+gallery.ID.String()+"/submit",
		selectionSession.Session, map[string]any{"expected_version": 1}, &submitted); err != nil {
		return uuid.Nil, err
	}
	if submitted.SubmittedAt == nil {
		return uuid.Nil, fmt.Errorf("selection submit did not stamp submitted_at")
	}

	var created createdRoundView
	if err := c.do(http.MethodPost, "/api/v1/proof-rounds", c.token, map[string]any{
		"project_id": projectID, "gallery_id": gallery.ID, "due_at": time.Now().Add(7 * 24 * time.Hour),
	}, &created); err != nil {
		return uuid.Nil, err
	}
	if created.Items != len(assetIDs) {
		return uuid.Nil, fmt.Errorf("proof round items = %d, want %d", created.Items, len(assetIDs))
	}
	var proofSession proofSessionView
	if err := c.do(http.MethodPost, "/g/v1/proof/session", "",
		map[string]any{"token": created.Token.Token}, &proofSession); err != nil {
		return uuid.Nil, err
	}
	var page proofManifestPage
	if err := c.do(http.MethodGet, "/g/v1/proof/rounds/"+created.Round.ID.String()+"/manifest?limit=60",
		proofSession.Session, nil, &page); err != nil {
		return uuid.Nil, err
	}
	var approved proofRoundView
	if err := c.do(http.MethodPost, "/g/v1/proof/rounds/"+created.Round.ID.String()+"/submit",
		proofSession.Session, map[string]any{
			"decision": "approved", "expected_version": page.Round.Version,
		}, &approved); err != nil {
		return uuid.Nil, err
	}
	if approved.Status != "approved" {
		return uuid.Nil, fmt.Errorf("proof approval = %s, want approved", approved.Status)
	}
	return created.Round.ID, nil
}

// payProject creates one deposit/balance order, pays it through the sandbox
// callback and returns the resulting payment row.
func (c *client) payProject(cfg smokeConfig, projectID uuid.UUID, kind string, amount int64, tag string) (*paymentRecord, error) {
	order, err := c.createOrder(projectID, kind)
	if err != nil {
		return nil, err
	}
	if order.AmountCents != amount {
		return nil, fmt.Errorf("%s order amount = %d, want %d", kind, order.AmountCents, amount)
	}
	event := map[string]any{
		"event_id": fmt.Sprintf("smoke-delivery-%s-%s", kind, tag), "type": payment.EventPaymentSucceeded,
		"merchant_id": cfg.PaymentMerchant, "out_trade_no": order.OutTradeNo,
		"provider_trade_no": fmt.Sprintf("wx-delivery-%s-%s", kind, tag),
		"amount_cents":      amount, "currency": "CNY", "paid_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.expectPaymentCallback(event, cfg.PaymentSecret, 200, "processed"); err != nil {
		return nil, err
	}
	payments, err := c.listPayments(projectID)
	if err != nil {
		return nil, err
	}
	for i := range payments.Items {
		if payments.Items[i].OrderID == order.ID {
			return &payments.Items[i], nil
		}
	}
	return nil, fmt.Errorf("%s payment for order %s missing", kind, order.ID)
}

func (c *client) waitDeliveryReady(deliveryID uuid.UUID) (*deliveryDetailView, error) {
	deadline := time.Now().Add(c.jobTimeout)
	for {
		var detail deliveryDetailView
		if err := c.do(http.MethodGet, "/api/v1/deliveries/"+deliveryID.String(), c.token, nil, &detail); err != nil {
			return nil, err
		}
		switch detail.Status {
		case "ready":
			return &detail, nil
		case "failed", "blocked":
			return nil, fmt.Errorf("delivery %s: %s %s", detail.Status, detail.LastError, detail.BlockedReason)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("delivery still %s after %s (is the worker running?)", detail.Status, c.jobTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (c *client) waitArchiveReady(archiveID uuid.UUID) (archiveView, error) {
	deadline := time.Now().Add(c.jobTimeout)
	for {
		var view archiveView
		if err := c.do(http.MethodGet, "/api/v1/archives/"+archiveID.String(), c.token, nil, &view); err != nil {
			return view, err
		}
		switch view.Status {
		case "ready":
			return view, nil
		case "failed":
			return view, fmt.Errorf("archive verification failed: %s", view.LastError)
		}
		if time.Now().After(deadline) {
			return view, fmt.Errorf("archive still %s after %s (is the worker running?)", view.Status, c.jobTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (c *client) transitionToArchived(projectID uuid.UUID) error {
	project, err := c.getProject(projectID)
	if err != nil {
		return err
	}
	version := project.Version
	for _, to := range []string{"quoted", "contract_pending", "contracted", "booked",
		"shooting", "post_production", "completed", "archived"} {
		var next commerceProject
		if err := c.do(http.MethodPost, "/api/v1/projects/"+projectID.String()+"/transitions", c.token,
			map[string]any{"to": to, "expected_version": version}, &next); err != nil {
			return fmt.Errorf("transition to %s: %w", to, err)
		}
		version = next.Version
	}
	return nil
}

func (c *client) deliverySession(token string) (*deliverySessionView, error) {
	var view deliverySessionView
	if err := c.do(http.MethodPost, "/g/v1/delivery/session", "",
		map[string]any{"token": token}, &view); err != nil {
		return nil, err
	}
	return &view, nil
}

func (c *client) download(session string, body map[string]any) (*deliveryGrantView, error) {
	var grant deliveryGrantView
	if err := c.do(http.MethodPost, "/g/v1/delivery/downloads", session, body, &grant); err != nil {
		return nil, err
	}
	if grant.URL == "" || grant.FileName == "" || grant.DownloadsRemaining < 0 {
		return nil, fmt.Errorf("download grant = %+v", grant)
	}
	return &grant, nil
}

// fetchGrant downloads the presigned object and insists on the expected size so
// a truncated transfer cannot pass the hash comparison silently.
func (c *client) fetchGrant(url string, wantSize int64) ([]byte, error) {
	status, _, body, err := c.fetchURL(url)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("presigned fetch = %d (%d bytes)", status, len(body))
	}
	if int64(len(body)) != wantSize {
		return nil, fmt.Errorf("presigned fetch = %d bytes, want %d", len(body), wantSize)
	}
	return body, nil
}

// zipManifest reads manifest.json out of a delivered ZIP.
func zipManifest(body []byte) (*deliveryManifestView, error) {
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	for _, file := range reader.File {
		if file.Name != "manifest.json" {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(rc, 8<<20))
		rc.Close()
		if err != nil {
			return nil, err
		}
		var manifest deliveryManifestView
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		return &manifest, nil
	}
	return nil, fmt.Errorf("archive has no manifest.json")
}

// verifyDeliveryEmail waits for the queued delivery mail to be sent and checks
// that the body (with the fragment link) was cleared afterwards.
func (c *client) verifyDeliveryEmail(cfg smokeConfig, deliveryID uuid.UUID) error {
	step("delivery: queue assertions")
	ctx, cancel := context.WithTimeout(context.Background(), c.jobTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database for delivery checks: %w", err)
	}
	defer pool.Close()

	deadline := time.Now().Add(c.jobTimeout)
	var sent int
	for {
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM email_messages
			WHERE related_type = 'delivery' AND related_id = $1 AND status = 'sent'`,
			deliveryID).Scan(&sent); err != nil {
			return fmt.Errorf("count sent delivery mail: %w", err)
		}
		if sent > 0 {
			break
		}
		if time.Now().After(deadline) {
			var summary string
			_ = pool.QueryRow(ctx, `
				SELECT coalesce(string_agg(status || '(' || attempts || ')', ', ' ORDER BY created_at), '')
				FROM email_messages WHERE related_type = 'delivery' AND related_id = $1`,
				deliveryID).Scan(&summary)
			return fmt.Errorf("no delivery email was sent after %s (queue: %s)", c.jobTimeout, summary)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	var leaked int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM email_messages
		WHERE related_type = 'delivery' AND related_id = $1 AND status = 'sent' AND body_text <> ''`,
		deliveryID).Scan(&leaked); err != nil {
		return fmt.Errorf("check cleared delivery bodies: %w", err)
	}
	if leaked > 0 {
		return fmt.Errorf("%d sent delivery emails kept their body; the link should be cleared after delivery", leaked)
	}
	fmt.Println("  delivery mail sent once and its body cleared")
	return nil
}
