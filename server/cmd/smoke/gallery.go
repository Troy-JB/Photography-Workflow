package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// runGallery exercises M5 end to end: derivative gate, publish, share token,
// session, manifest pages, signed image URLs, selection conflicts, limits,
// submission, revocation and the staff result views.
func runGallery(c *client, cfg smokeConfig) error {
	step("gallery: create project")
	var project struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects", c.token,
		map[string]any{"title": cfg.Title + " gallery"}, &project); err != nil {
		return err
	}

	contents := make([][]byte, 3)
	files := make([]map[string]any, len(contents))
	for i := range contents {
		body, err := galleryFixture(800, 600, float64(i)*0.3)
		if err != nil {
			return fmt.Errorf("encode fixture %d: %w", i, err)
		}
		contents[i] = body
		sum := sha256.Sum256(body)
		files[i] = map[string]any{
			"rel_path": fmt.Sprintf("gallery/GAL_%02d.JPG", i+1),
			"size":     len(body),
			"sha256":   hex.EncodeToString(sum[:]),
			"mime":     "image/jpeg",
			"width":    800,
			"height":   600,
		}
	}

	step("gallery: upload three JPEGs")
	var batch struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches", c.token,
		map[string]any{"project_id": project.ID, "device_id": "smoke-gallery"}, &batch); err != nil {
		return err
	}
	var registered registerResult
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/assets/batch", c.token,
		map[string]any{"files": files}, &registered); err != nil {
		return err
	}
	if registered.Accepted != 3 || len(registered.Assets) != 3 {
		return fmt.Errorf("register gallery files = accepted %d assets %d, want 3", registered.Accepted, len(registered.Assets))
	}
	assetIDs := make([]uuid.UUID, 3)
	for i, entry := range registered.Assets {
		assetIDs[i] = entry.AssetID
		plan, err := c.presign(entry.AssetID, int64(len(contents[i])))
		if err != nil {
			return err
		}
		if plan.Mode != "single" {
			return fmt.Errorf("gallery fixture used %s upload mode, want single", plan.Mode)
		}
		if err := c.putFile(plan.URL, contents[i]); err != nil {
			return err
		}
		var asset assetMeta
		if err := c.do(http.MethodPost, "/agent/v1/uploads/"+entry.AssetID.String()+"/complete", c.token,
			map[string]any{"upload_id": plan.UploadID, "parts": []completePart{}}, &asset); err != nil {
			return err
		}
		if asset.Status != "uploaded" {
			return fmt.Errorf("gallery asset %d status = %s, want uploaded", i, asset.Status)
		}
	}
	var finalized finalizeJob
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/finalize", c.token,
		nil, &finalized); err != nil {
		return err
	}
	job, err := c.waitJob(finalized.ID)
	if err != nil {
		return err
	}
	if job.Result.Verified != 3 || job.Result.Missing != 0 || job.Result.Corrupt != 0 {
		return fmt.Errorf("gallery verify result = %+v, want 3 verified", job.Result)
	}
	fmt.Printf("  3 JPEGs uploaded and verified (project %s)\n", project.ID)

	step("gallery: create draft and attach assets")
	var gallery galleryView
	if err := c.do(http.MethodPost, "/api/v1/galleries", c.token, map[string]any{
		"project_id":      project.ID,
		"title":           "Smoke selection " + shortRandom(),
		"expires_at":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"selection_limit": 1,
	}, &gallery); err != nil {
		return err
	}
	if gallery.Status != "draft" || gallery.RoundNo == 0 {
		return fmt.Errorf("new gallery status=%s round=%d, want draft with a round", gallery.Status, gallery.RoundNo)
	}
	galleryPath := "/api/v1/galleries/" + gallery.ID.String()
	var attached struct {
		Added   int         `json:"added"`
		Skipped int         `json:"skipped"`
		Gallery galleryView `json:"gallery"`
	}
	if err := c.do(http.MethodPost, galleryPath+"/assets", c.token,
		map[string]any{"asset_ids": assetIDs}, &attached); err != nil {
		return err
	}
	if attached.Added != 3 {
		return fmt.Errorf("attach = added %d, want 3", attached.Added)
	}
	var removed galleryView
	if err := c.do(http.MethodDelete, galleryPath+"/assets/"+assetIDs[2].String(), c.token, nil, &removed); err != nil {
		return err
	}
	var candidates candidatePage
	if err := c.do(http.MethodGet, galleryPath+"/candidates?q=GAL_&limit=50", c.token, nil, &candidates); err != nil {
		return err
	}
	inGallery := map[uuid.UUID]bool{}
	for _, item := range candidates.Items {
		inGallery[item.AssetID] = item.InGallery
	}
	if candidates.Total != 3 || !inGallery[assetIDs[0]] || !inGallery[assetIDs[1]] || inGallery[assetIDs[2]] {
		return fmt.Errorf("candidates total=%d membership=%v, want 2 attached and 1 free", candidates.Total, inGallery)
	}
	fmt.Println("  attach 3, remove 1, candidate flags follow the gallery")

	step("gallery: publish is gated on derivatives")
	if err := c.expect(http.MethodPost, galleryPath+"/publish", c.token, nil, http.StatusConflict, "derivatives_incomplete"); err != nil {
		return err
	}
	var derived struct {
		Candidates int    `json:"candidates"`
		Watermark  string `json:"watermark"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects/"+project.ID.String()+"/derivatives:derive", c.token,
		map[string]any{"asset_ids": []uuid.UUID{}, "force": false}, &derived); err != nil {
		return err
	}
	if derived.Watermark == "" {
		return fmt.Errorf("derive response has no watermark")
	}
	if err := c.waitDerivatives(project.ID, assetIDs[:2]); err != nil {
		return err
	}
	fmt.Printf("  publish without previews -> 409, watermark=%q, thumb/display ready\n", derived.Watermark)

	step("gallery: publish and freeze")
	if err := c.do(http.MethodPost, galleryPath+"/publish", c.token, nil, &gallery); err != nil {
		return err
	}
	if gallery.Status != "published" || gallery.ManifestKey == "" || gallery.ManifestVersion < 1 {
		return fmt.Errorf("published gallery = status %s key %q version %d", gallery.Status, gallery.ManifestKey, gallery.ManifestVersion)
	}
	version := gallery.ManifestVersion
	if err := c.do(http.MethodPost, galleryPath+"/publish", c.token, nil, &gallery); err != nil {
		return err
	}
	if gallery.ManifestVersion != version {
		return fmt.Errorf("re-publish bumped manifest version %d -> %d", version, gallery.ManifestVersion)
	}
	if err := c.expect(http.MethodPatch, galleryPath, c.token, map[string]any{
		"expected_version": gallery.Version,
		"watermark_text":   "changed after publish",
	}, http.StatusConflict, "invalid_state"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, galleryPath+"/assets", c.token,
		map[string]any{"asset_ids": []uuid.UUID{assetIDs[2]}}, http.StatusConflict, "invalid_state"); err != nil {
		return err
	}
	fmt.Printf("  published v%d, re-publish idempotent, asset/watermark frozen\n", version)

	step("gallery: share token and session")
	var issued issuedShare
	if err := c.do(http.MethodPost, galleryPath+"/tokens", c.token,
		map[string]any{"label": "smoke", "max_visits": 1}, &issued); err != nil {
		return err
	}
	if len(issued.Token) < 32 || !strings.Contains(issued.GalleryURL, "#/a/"+issued.Token) {
		return fmt.Errorf("share token %q url %q is not a fragment link", issued.Token, issued.GalleryURL)
	}
	if strings.Contains(strings.SplitN(issued.GalleryURL, "#", 2)[0], "?") {
		return fmt.Errorf("share url leaks the token into the query string: %s", issued.GalleryURL)
	}
	var firstSession gallerySession
	if err := c.do(http.MethodPost, "/g/v1/session", "", map[string]any{"token": issued.Token}, &firstSession); err != nil {
		return err
	}
	if firstSession.Session == "" || firstSession.Session == issued.Token {
		return fmt.Errorf("session must be a separate credential")
	}
	if firstSession.Album.ID != gallery.ID {
		return fmt.Errorf("session album %s, want %s", firstSession.Album.ID, gallery.ID)
	}
	if err := c.expect(http.MethodPost, "/g/v1/session", "",
		map[string]any{"token": issued.Token}, http.StatusTooManyRequests, "too_many_visits"); err != nil {
		return err
	}
	fmt.Println("  fragment link exchanged for a session; visit cap enforced")

	step("gallery: manifest pages and signed images")
	var page manifestPage
	status, headers, raw, err := c.galleryCall(http.MethodGet,
		"/g/v1/albums/"+gallery.ID.String()+"/manifest?limit=1", firstSession.Session, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("manifest: %d: %s", status, string(raw))
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("decode manifest page: %w", err)
	}
	if headers.Get("Cache-Control") != "no-store" || headers.Get("Referrer-Policy") != "no-referrer" {
		return fmt.Errorf("manifest headers cache=%q referrer=%q", headers.Get("Cache-Control"), headers.Get("Referrer-Policy"))
	}
	if len(page.Items) != 1 || page.NextCursor == "" || page.SelectionVersion != 0 {
		return fmt.Errorf("manifest page 1 = %d items cursor %q version %d", len(page.Items), page.NextCursor, page.SelectionVersion)
	}
	ids := map[uuid.UUID]bool{}
	var firstItem manifestItem
	for cursor := page.NextCursor; ; {
		var next manifestPage
		path := "/g/v1/albums/" + gallery.ID.String() + "/manifest?limit=1&cursor=" + cursor
		if err := c.do(http.MethodGet, path, firstSession.Session, nil, &next); err != nil {
			return err
		}
		if firstItem.AssetID == uuid.Nil {
			firstItem = page.Items[0]
		}
		for _, item := range page.Items {
			ids[item.AssetID] = true
		}
		page = next
		ids[next.Items[0].AssetID] = true
		if next.NextCursor == "" {
			break
		}
		cursor = next.NextCursor
	}
	if len(ids) != 2 || ids[assetIDs[2]] {
		return fmt.Errorf("manifest listed %d assets (%v), want exactly the 2 attached", len(ids), ids)
	}
	thumbStatus, thumbHeader, thumbBytes, err := c.fetchURL(firstItem.ThumbURL)
	if err != nil {
		return err
	}
	if thumbStatus != http.StatusOK || thumbHeader.Get("Content-Type") == "" || len(thumbBytes) < 512 {
		return fmt.Errorf("thumb fetch: status %d type %q bytes %d", thumbStatus, thumbHeader.Get("Content-Type"), len(thumbBytes))
	}
	thumbCfg, err := jpeg.DecodeConfig(bytes.NewReader(thumbBytes))
	if err != nil {
		return fmt.Errorf("decode thumb: %w", err)
	}
	if thumbCfg.Width > 640 || thumbCfg.Height > 640 {
		return fmt.Errorf("thumb %dx%d exceeds the 640px long edge", thumbCfg.Width, thumbCfg.Height)
	}
	displayStatus, _, displayBytes, err := c.fetchURL(firstItem.DisplayURL)
	if err != nil {
		return err
	}
	if displayStatus != http.StatusOK {
		return fmt.Errorf("display fetch: status %d", displayStatus)
	}
	displayCfg, err := jpeg.DecodeConfig(bytes.NewReader(displayBytes))
	if err != nil {
		return fmt.Errorf("decode display: %w", err)
	}
	if displayCfg.Width > 2048 || displayCfg.Height > 2048 {
		return fmt.Errorf("display %dx%d exceeds the 2048px long edge", displayCfg.Width, displayCfg.Height)
	}
	if bytes.Equal(displayBytes, thumbBytes) {
		return fmt.Errorf("display equals the thumb; the watermark pass did not run")
	}
	fmt.Printf("  signed thumb %dx%d and display %dx%d fetched straight from storage\n",
		thumbCfg.Width, thumbCfg.Height, displayCfg.Width, displayCfg.Height)

	step("gallery: selections, conflicts and limits")
	idem := map[string]string{"Idempotency-Key": "smoke-gallery-" + gallery.ID.String()}
	selectBody := map[string]any{
		"expected_version": 0,
		"items": []map[string]any{
			{"asset_id": assetIDs[0], "decision": "selected"},
			{"asset_id": assetIDs[1], "decision": "rejected"},
		},
	}
	var selection selectionResult
	if err := c.doHeaders(http.MethodPost, "/g/v1/selections", firstSession.Session, idem, selectBody, &selection); err != nil {
		return err
	}
	if selection.SelectionVersion != 1 || selection.Counts.Selected != 1 || selection.Counts.Rejected != 1 {
		return fmt.Errorf("selection result = version %d counts %+v", selection.SelectionVersion, selection.Counts)
	}
	var replay selectionResult
	if err := c.doHeaders(http.MethodPost, "/g/v1/selections", firstSession.Session, idem, selectBody, &replay); err != nil {
		return err
	}
	if replay != selection {
		return fmt.Errorf("idempotent replay changed the result: %+v -> %+v", selection, replay)
	}
	status, _, raw, err = c.galleryCall(http.MethodPost, "/g/v1/selections", firstSession.Session, nil, map[string]any{
		"expected_version": 0,
		"items":            []map[string]any{{"asset_id": assetIDs[1], "decision": "selected"}},
	})
	if err != nil {
		return err
	}
	var problem galleryProblem
	_ = json.Unmarshal(raw, &problem)
	if status != http.StatusConflict || problem.Code != "version_conflict" || problem.CurrentVersion != 1 {
		return fmt.Errorf("stale selection = %d %q current=%d", status, problem.Code, problem.CurrentVersion)
	}
	if err := c.expect(http.MethodPost, "/g/v1/selections", firstSession.Session, map[string]any{
		"expected_version": 1,
		"items":            []map[string]any{{"asset_id": assetIDs[1], "decision": "selected"}},
	}, http.StatusConflict, "selection_limit_exceeded"); err != nil {
		return err
	}
	fmt.Println("  batch saved as v1, replay idempotent, stale version 409, limit 409")

	step("gallery: submit freezes decisions")
	var album galleryAlbum
	if err := c.do(http.MethodPost, "/g/v1/albums/"+gallery.ID.String()+"/submit",
		firstSession.Session, map[string]any{"expected_version": 1}, &album); err != nil {
		return err
	}
	if album.SubmittedAt == nil {
		return fmt.Errorf("submit did not stamp submitted_at")
	}
	submitted := *album.SubmittedAt
	var again galleryAlbum
	if err := c.do(http.MethodPost, "/g/v1/albums/"+gallery.ID.String()+"/submit",
		firstSession.Session, map[string]any{"expected_version": 1}, &again); err != nil {
		return err
	}
	if again.SubmittedAt == nil || !again.SubmittedAt.Equal(submitted) {
		return fmt.Errorf("repeat submit moved submitted_at %v -> %v", submitted, again.SubmittedAt)
	}
	if err := c.expect(http.MethodPost, "/g/v1/selections", firstSession.Session, map[string]any{
		"expected_version": 1,
		"items":            []map[string]any{{"asset_id": assetIDs[0], "decision": "none"}},
	}, http.StatusConflict, "already_submitted"); err != nil {
		return err
	}
	fmt.Println("  submitted_at set once, repeat submit idempotent, edits rejected")

	step("gallery: staff result views")
	var staffAssets galleryAssetsPage
	if err := c.do(http.MethodGet, galleryPath+"/assets?filter=all&limit=50", c.token, nil, &staffAssets); err != nil {
		return err
	}
	if staffAssets.Total != 2 || staffAssets.Derivatives.Missing != 0 {
		return fmt.Errorf("staff assets total=%d missing=%d", staffAssets.Total, staffAssets.Derivatives.Missing)
	}
	decisions := map[uuid.UUID]string{}
	for _, item := range staffAssets.Items {
		decisions[item.AssetID] = item.Decision
		if item.ThumbURL == "" || !item.PreviewReady {
			return fmt.Errorf("staff asset %s thumb=%q ready=%v", item.AssetID, item.ThumbURL, item.PreviewReady)
		}
	}
	if decisions[assetIDs[0]] != "selected" || decisions[assetIDs[1]] != "rejected" {
		return fmt.Errorf("staff decisions = %v", decisions)
	}
	var selectedPage galleryAssetsPage
	if err := c.do(http.MethodGet, galleryPath+"/assets?filter=selected", c.token, nil, &selectedPage); err != nil {
		return err
	}
	if selectedPage.Total != 1 {
		return fmt.Errorf("filter=selected total=%d, want 1", selectedPage.Total)
	}
	var notificationPage struct {
		Total int `json:"total"`
		Items []struct {
			Type      string    `json:"type"`
			SubjectID uuid.UUID `json:"subject_id"`
		} `json:"items"`
	}
	if err := c.do(http.MethodGet, "/api/v1/notifications?type=gallery_submitted&limit=100", c.token, nil, &notificationPage); err != nil {
		return err
	}
	notified := false
	for _, item := range notificationPage.Items {
		if item.SubjectID == gallery.ID && item.Type == "gallery_submitted" {
			notified = true
		}
	}
	if !notified {
		return fmt.Errorf("gallery_submitted notification for %s missing", gallery.ID)
	}
	fmt.Println("  decisions visible to staff, selected filter works, submission notified")

	step("gallery: revoke token kills the live session")
	if err := c.expect(http.MethodDelete, galleryPath+"/tokens/"+issued.ID.String(), c.token, nil,
		http.StatusNoContent, ""); err != nil {
		return err
	}
	if err := c.expect(http.MethodGet, "/g/v1/session", firstSession.Session, nil,
		http.StatusUnauthorized, "session_revoked"); err != nil {
		return err
	}
	if err := c.expect(http.MethodGet, "/g/v1/albums/"+gallery.ID.String()+"/manifest", firstSession.Session,
		nil, http.StatusUnauthorized, "session_revoked"); err != nil {
		return err
	}
	fmt.Println("  revoked token invalidates the session immediately")

	step("gallery: reopen lets the customer revise")
	if err := c.do(http.MethodPost, galleryPath+"/reopen", c.token, nil, &gallery); err != nil {
		return err
	}
	if gallery.SubmittedAt != nil {
		return fmt.Errorf("reopen kept submitted_at %v", gallery.SubmittedAt)
	}
	var second issuedShare
	if err := c.do(http.MethodPost, galleryPath+"/tokens", c.token,
		map[string]any{"label": "reopen"}, &second); err != nil {
		return err
	}
	var secondSession gallerySession
	if err := c.do(http.MethodPost, "/g/v1/session", "", map[string]any{"token": second.Token}, &secondSession); err != nil {
		return err
	}
	var revised selectionResult
	if err := c.do(http.MethodPost, "/g/v1/selections", secondSession.Session, map[string]any{
		"expected_version": 1,
		"items": []map[string]any{
			{"asset_id": assetIDs[0], "decision": "none"},
			{"asset_id": assetIDs[1], "decision": "rejected"},
		},
	}, &revised); err != nil {
		return err
	}
	if revised.SelectionVersion != 2 || revised.Counts.Selected != 0 || revised.Counts.Rejected != 1 {
		return fmt.Errorf("revised selection = version %d counts %+v", revised.SelectionVersion, revised.Counts)
	}
	fmt.Println("  reopened, new link, decisions revised to v2")

	step("gallery: revoke album")
	if err := c.do(http.MethodPost, galleryPath+"/revoke", c.token, nil, &gallery); err != nil {
		return err
	}
	if gallery.Status != "revoked" {
		return fmt.Errorf("revoke left status %s", gallery.Status)
	}
	if err := c.expect(http.MethodGet, "/g/v1/session", secondSession.Session, nil,
		http.StatusGone, "gallery_revoked"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/g/v1/session", "",
		map[string]any{"token": second.Token}, http.StatusGone, "revoked"); err != nil {
		return err
	}
	fmt.Println("  revoked album answers 410 for the session and the link")
	return nil
}

type galleryView struct {
	ID               uuid.UUID  `json:"id"`
	ProjectID        uuid.UUID  `json:"project_id"`
	Title            string     `json:"title"`
	Status           string     `json:"status"`
	RoundNo          int        `json:"round_no"`
	ManifestVersion  int        `json:"manifest_version"`
	SelectionLimit   *int       `json:"selection_limit"`
	ManifestKey      string     `json:"manifest_key"`
	Version          int        `json:"version"`
	SelectionVersion int        `json:"selection_version"`
	SubmittedAt      *time.Time `json:"submitted_at"`
}

type selectionCounts struct {
	Assets    int `json:"assets"`
	Selected  int `json:"selected"`
	Rejected  int `json:"rejected"`
	Undecided int `json:"undecided"`
}

type selectionResult struct {
	SelectionVersion int             `json:"selection_version"`
	Counts           selectionCounts `json:"counts"`
}

type galleryAlbum struct {
	ID          uuid.UUID       `json:"id"`
	Title       string          `json:"title"`
	SubmittedAt *time.Time      `json:"submitted_at"`
	Counts      selectionCounts `json:"counts"`
}

type gallerySession struct {
	Session   string       `json:"session"`
	ExpiresAt time.Time    `json:"expires_at"`
	Album     galleryAlbum `json:"album"`
}

type issuedShare struct {
	ID         uuid.UUID `json:"id"`
	Token      string    `json:"token"`
	GalleryURL string    `json:"gallery_url"`
}

type manifestItem struct {
	AssetID    uuid.UUID `json:"asset_id"`
	Filename   string    `json:"filename"`
	Position   int       `json:"position"`
	ThumbURL   string    `json:"thumb_url"`
	DisplayURL string    `json:"display_url"`
	LQIP       string    `json:"lqip"`
	Decision   string    `json:"decision"`
}

type manifestPage struct {
	Items            []manifestItem `json:"items"`
	NextCursor       string         `json:"next_cursor"`
	SelectionVersion int            `json:"selection_version"`
}

type galleryProblem struct {
	Code           string `json:"code"`
	Detail         string `json:"detail"`
	CurrentVersion int    `json:"current_version"`
}

type candidatePage struct {
	Total int `json:"total"`
	Items []struct {
		AssetID   uuid.UUID `json:"asset_id"`
		InGallery bool      `json:"in_gallery"`
	} `json:"items"`
}

type galleryAssetsPage struct {
	Total       int `json:"total"`
	Derivatives struct {
		Missing int `json:"missing"`
		Failed  int `json:"failed"`
	} `json:"derivatives"`
	Items []struct {
		AssetID      uuid.UUID `json:"asset_id"`
		Filename     string    `json:"filename"`
		Decision     string    `json:"decision"`
		PreviewReady bool      `json:"preview_ready"`
		ThumbURL     string    `json:"thumb_url"`
	} `json:"items"`
}

// waitDerivatives polls the project asset list until every id reports
// derivative_ready, which means the worker finished thumb/display/LQIP.
func (c *client) waitDerivatives(projectID uuid.UUID, ids []uuid.UUID) error {
	wanted := map[uuid.UUID]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	deadline := time.Now().Add(c.jobTimeout)
	for {
		var listed struct {
			Items []struct {
				ID     uuid.UUID `json:"id"`
				Status string    `json:"status"`
			} `json:"items"`
		}
		if err := c.do(http.MethodGet, "/api/v1/projects/"+projectID.String()+"/assets?limit=200",
			c.token, nil, &listed); err != nil {
			return err
		}
		ready := 0
		for _, item := range listed.Items {
			if wanted[item.ID] && item.Status == "derivative_ready" {
				ready++
			}
		}
		if ready == len(wanted) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d assets still lack derivatives after %s (is the worker running?)",
				len(wanted)-ready, len(wanted), c.jobTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// galleryCall is like doStatusHeaders but keeps the response headers so tests
// can assert the no-store / no-referrer contract.
func (c *client) galleryCall(method, path, token string, headers map[string]string, body any) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, nil, err
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
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// fetchURL loads a presigned image without any API credentials, proving the
// bytes come straight from object storage.
func (c *client) fetchURL(url string) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, raw, nil
}

func galleryFixture(w, h int, phase float64) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			fy := float64(y) / float64(h)
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(255 * (0.25 + 0.65*fx)),
				G: uint8(255 * (0.2 + 0.7*fy)),
				B: uint8(255 * (0.35 + 0.45*(1-fx) + 0.2*phase)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
