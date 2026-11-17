package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runProof exercises M6 end to end: unlimited proof review cycles, independent
// proof links, the revision-aware manifest, annotations, revocation and mail.
func runProof(c *client, cfg smokeConfig) error {
	step("proof: customer and project with an email")
	var customer struct {
		ID    uuid.UUID `json:"id"`
		Email string    `json:"email"`
	}
	email := fmt.Sprintf("proof+%s@example.com", strings.ToLower(shortRandom()))
	if err := c.do(http.MethodPost, "/api/v1/clients", c.token,
		map[string]any{"name": "Smoke Customer", "email": email}, &customer); err != nil {
		return err
	}
	var project struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects", c.token,
		map[string]any{"title": cfg.Title + " proof", "client_id": customer.ID}, &project); err != nil {
		return err
	}
	fmt.Printf("  client %s (%s), project %s\n", customer.ID, email, project.ID)

	contents := make([][]byte, 2)
	files := make([]map[string]any, len(contents))
	for i := range contents {
		body, err := galleryFixture(800, 600, 0.55+float64(i)*0.2)
		if err != nil {
			return fmt.Errorf("encode proof fixture %d: %w", i, err)
		}
		contents[i] = body
		sum := sha256.Sum256(body)
		files[i] = map[string]any{
			"rel_path": fmt.Sprintf("proof/PRF_%02d.JPG", i+1),
			"size":     len(body),
			"sha256":   hex.EncodeToString(sum[:]),
			"mime":     "image/jpeg",
			"width":    800,
			"height":   600,
		}
	}

	step("proof: upload two JPEGs and publish the gallery")
	var batch struct {
		ID uuid.UUID `json:"id"`
	}
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches", c.token,
		map[string]any{"project_id": project.ID, "device_id": "smoke-proof"}, &batch); err != nil {
		return err
	}
	var registered registerResult
	if err := c.do(http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID.String()+"/assets/batch", c.token,
		map[string]any{"files": files}, &registered); err != nil {
		return err
	}
	if registered.Accepted != 2 || len(registered.Assets) != 2 {
		return fmt.Errorf("register proof files = accepted %d assets %d, want 2", registered.Accepted, len(registered.Assets))
	}
	assetIDs := make([]uuid.UUID, 2)
	for i, entry := range registered.Assets {
		assetIDs[i] = entry.AssetID
		plan, err := c.presign(entry.AssetID, int64(len(contents[i])))
		if err != nil {
			return err
		}
		if plan.Mode != "single" {
			return fmt.Errorf("proof fixture used %s upload mode, want single", plan.Mode)
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
			return fmt.Errorf("proof asset %d status = %s, want uploaded", i, asset.Status)
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
	if job.Result.Verified != 2 || job.Result.Missing != 0 || job.Result.Corrupt != 0 {
		return fmt.Errorf("proof verify result = %+v, want 2 verified", job.Result)
	}

	var gallery galleryView
	if err := c.do(http.MethodPost, "/api/v1/galleries", c.token, map[string]any{
		"project_id":      project.ID,
		"title":           "Smoke proof " + shortRandom(),
		"expires_at":      time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"selection_limit": 2,
	}, &gallery); err != nil {
		return err
	}
	galleryPath := "/api/v1/galleries/" + gallery.ID.String()
	var attached struct {
		Added int `json:"added"`
	}
	if err := c.do(http.MethodPost, galleryPath+"/assets", c.token,
		map[string]any{"asset_ids": assetIDs}, &attached); err != nil {
		return err
	}
	if attached.Added != 2 {
		return fmt.Errorf("proof gallery attach = %d, want 2", attached.Added)
	}
	var derived struct {
		Candidates int    `json:"candidates"`
		Watermark  string `json:"watermark"`
	}
	if err := c.do(http.MethodPost, "/api/v1/projects/"+project.ID.String()+"/derivatives:derive", c.token,
		map[string]any{"asset_ids": []uuid.UUID{}, "force": false}, &derived); err != nil {
		return err
	}
	if err := c.waitDerivatives(project.ID, assetIDs); err != nil {
		return err
	}
	if err := c.do(http.MethodPost, galleryPath+"/publish", c.token, nil, &gallery); err != nil {
		return err
	}
	if gallery.Status != "published" {
		return fmt.Errorf("proof gallery status = %s, want published", gallery.Status)
	}
	fmt.Printf("  two previews derived, gallery %s published (watermark %q)\n", gallery.ID, derived.Watermark)

	step("proof: customer selects and submits")
	var share issuedShare
	if err := c.do(http.MethodPost, galleryPath+"/tokens", c.token,
		map[string]any{"label": "smoke proof"}, &share); err != nil {
		return err
	}
	var selectionSession gallerySession
	if err := c.do(http.MethodPost, "/g/v1/session", "",
		map[string]any{"token": share.Token}, &selectionSession); err != nil {
		return err
	}
	var selection selectionResult
	if err := c.do(http.MethodPost, "/g/v1/selections", selectionSession.Session, map[string]any{
		"expected_version": 0,
		"items": []map[string]any{
			{"asset_id": assetIDs[0], "decision": "selected"},
			{"asset_id": assetIDs[1], "decision": "selected"},
		},
	}, &selection); err != nil {
		return err
	}
	if selection.Counts.Selected != 2 {
		return fmt.Errorf("proof selection counts = %+v, want 2 selected", selection.Counts)
	}
	var submitted galleryAlbum
	if err := c.do(http.MethodPost, "/g/v1/albums/"+gallery.ID.String()+"/submit",
		selectionSession.Session, map[string]any{"expected_version": 1}, &submitted); err != nil {
		return err
	}
	if submitted.SubmittedAt == nil {
		return fmt.Errorf("proof selection submit did not stamp submitted_at")
	}
	fmt.Println("  two photos selected and the selection submitted")

	step("proof: create included rounds")
	due := time.Now().Add(7 * 24 * time.Hour)
	roundPath := func(id uuid.UUID) string { return "/api/v1/proof-rounds/" + id.String() }
	createRound := func(body map[string]any) (*createdRoundView, error) {
		var created createdRoundView
		if err := c.do(http.MethodPost, "/api/v1/proof-rounds", c.token, body, &created); err != nil {
			return nil, err
		}
		return &created, nil
	}
	baseBody := map[string]any{"project_id": project.ID, "gallery_id": gallery.ID, "due_at": due}
	first, err := createRound(baseBody)
	if err != nil {
		return err
	}
	if first.Round.RoundNo != 1 || first.Round.Status != "open" || first.Items != 2 {
		return fmt.Errorf("first proof round = no %d status %s items %d", first.Round.RoundNo, first.Round.Status, first.Items)
	}
	if len(first.Token.Token) < 32 || !strings.Contains(first.Token.ProofURL, "#/p/"+first.Token.Token) {
		return fmt.Errorf("proof link %q is not a fragment link", first.Token.ProofURL)
	}
	if first.Round.ClientEmail != email {
		return fmt.Errorf("round client email = %q, want %q", first.Round.ClientEmail, email)
	}
	second, err := createRound(baseBody)
	if err != nil {
		return err
	}
	if second.Round.RoundNo != 2 {
		return fmt.Errorf("second proof round number = %d, want 2", second.Round.RoundNo)
	}
	step("proof: create additional round without contract gates")
	third, err := createRound(baseBody)
	if err != nil {
		return err
	}
	if third.Round.RoundNo != 3 || third.Round.Status != "open" {
		return fmt.Errorf("third proof round = no %d status %s, want 3/open", third.Round.RoundNo, third.Round.Status)
	}
	fmt.Println("  third round created without a contract or change-order gate")

	step("proof: customer session and revision-aware manifest")
	var proofSession proofSessionView
	if err := c.do(http.MethodPost, "/g/v1/proof/session", "",
		map[string]any{"token": first.Token.Token}, &proofSession); err != nil {
		return err
	}
	if proofSession.Session == "" || proofSession.Session == first.Token.Token {
		return fmt.Errorf("proof session must be a separate credential")
	}
	if proofSession.Round.ID != first.Round.ID {
		return fmt.Errorf("proof session round %s, want %s", proofSession.Round.ID, first.Round.ID)
	}
	if err := c.expect(http.MethodPost, "/g/v1/proof/session", "",
		map[string]any{"token": "not-a-real-token"}, http.StatusUnauthorized, "invalid_token"); err != nil {
		return err
	}

	var page proofManifestPage
	status, headers, raw, err := c.galleryCall(http.MethodGet,
		"/g/v1/proof/rounds/"+first.Round.ID.String()+"/manifest?limit=1", proofSession.Session, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("proof manifest: %d: %s", status, string(raw))
	}
	if headers.Get("Cache-Control") != "no-store" || headers.Get("Referrer-Policy") != "no-referrer" {
		return fmt.Errorf("proof manifest headers cache=%q referrer=%q",
			headers.Get("Cache-Control"), headers.Get("Referrer-Policy"))
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("decode proof manifest: %w", err)
	}
	if page.Round.Status != "in_review" {
		return fmt.Errorf("manifest did not move the round to in_review: %s", page.Round.Status)
	}
	if page.Total != 2 || len(page.Items) != 1 || page.NextCursor == "" {
		return fmt.Errorf("manifest page 1 = total %d items %d cursor %q", page.Total, len(page.Items), page.NextCursor)
	}
	reviewVersion := page.Round.Version
	seen := map[uuid.UUID]bool{}
	firstItem := page.Items[0]
	for {
		for _, item := range page.Items {
			if item.ThumbURL == "" || item.DisplayURL == "" {
				return fmt.Errorf("manifest item %s has no signed previews", item.AssetID)
			}
			if item.RevisionNo != 0 {
				return fmt.Errorf("manifest item %s starts at revision %d, want the original", item.AssetID, item.RevisionNo)
			}
			seen[item.AssetID] = true
		}
		if page.NextCursor == "" {
			break
		}
		var next proofManifestPage
		if err := c.do(http.MethodGet, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/manifest?limit=1&cursor="+page.NextCursor,
			proofSession.Session, nil, &next); err != nil {
			return err
		}
		page = next
	}
	if len(seen) != 2 {
		return fmt.Errorf("manifest listed %d assets, want the 2 selected", len(seen))
	}
	thumbStatus, _, thumbBytes, err := c.fetchURL(firstItem.ThumbURL)
	if err != nil {
		return err
	}
	displayStatus, _, displayBytes, err := c.fetchURL(firstItem.DisplayURL)
	if err != nil {
		return err
	}
	if thumbStatus != http.StatusOK || displayStatus != http.StatusOK || len(thumbBytes) < 512 || bytes.Equal(thumbBytes, displayBytes) {
		return fmt.Errorf("proof previews thumb=%d display=%d equal=%v", thumbStatus, displayStatus, bytes.Equal(thumbBytes, displayBytes))
	}
	fmt.Println("  session issued, first manifest read opens the round, both pages served with signed previews")

	step("proof: annotations, idempotent comments and resolution")
	badAnnotation := map[string]any{
		"asset_id": assetIDs[0],
		"body":     "reversed corners",
		"annotation": map[string]any{
			"schema": "annotation.v1", "type": "rect",
			"points": []map[string]any{{"x": 0.8, "y": 0.2}, {"x": 0.2, "y": 0.8}},
		},
	}
	commentPath := "/g/v1/proof/rounds/" + first.Round.ID.String() + "/comments"
	status, _, raw, err = c.galleryCall(http.MethodPost, commentPath, proofSession.Session,
		map[string]string{"Idempotency-Key": "smoke-proof-bad-" + first.Round.ID.String()}, badAnnotation)
	if err != nil {
		return err
	}
	var problem proofProblem
	_ = json.Unmarshal(raw, &problem)
	if status != http.StatusUnprocessableEntity || problem.Code != "invalid_annotation" {
		return fmt.Errorf("bad annotation = %d %q, want 422 invalid_annotation", status, problem.Code)
	}

	idemKey := "smoke-proof-comment-" + first.Round.ID.String()
	commentBody := map[string]any{
		"asset_id": assetIDs[0],
		"body":     "请把天空压暗一点",
		"annotation": map[string]any{
			"schema": "annotation.v1", "type": "rect",
			"points": []map[string]any{{"x": 0.1, "y": 0.1}, {"x": 0.6, "y": 0.55}},
			"color":  "#FF0000",
		},
	}
	var comment proofCommentView
	if err := c.doHeaders(http.MethodPost, commentPath, proofSession.Session,
		map[string]string{"Idempotency-Key": idemKey}, commentBody, &comment); err != nil {
		return err
	}
	if comment.ID == uuid.Nil || len(comment.Annotation) == 0 || comment.AuthorType != "client" {
		return fmt.Errorf("comment = %+v, want a client annotation", comment)
	}
	var replay proofCommentView
	if err := c.doHeaders(http.MethodPost, commentPath, proofSession.Session,
		map[string]string{"Idempotency-Key": idemKey}, commentBody, &replay); err != nil {
		return err
	}
	if replay.ID != comment.ID {
		return fmt.Errorf("idempotent comment replay created %s, want %s", replay.ID, comment.ID)
	}

	var reply proofCommentView
	if err := c.do(http.MethodPost, commentPath, proofSession.Session, map[string]any{
		"asset_id": assetIDs[0], "parent_id": comment.ID, "body": "收到，稍后处理",
	}, &reply); err != nil {
		return err
	}
	if reply.ParentID == nil || *reply.ParentID != comment.ID {
		return fmt.Errorf("reply parent = %v, want %s", reply.ParentID, comment.ID)
	}

	resolvePath := roundPath(first.Round.ID) + "/comments/" + comment.ID.String() + "/resolve"
	var resolved proofCommentView
	if err := c.do(http.MethodPost, resolvePath, c.token, nil, &resolved); err != nil {
		return err
	}
	if resolved.ResolvedAt == nil {
		return fmt.Errorf("resolve did not stamp resolved_at")
	}
	var resolvedAgain proofCommentView
	if err := c.do(http.MethodPost, resolvePath, c.token, nil, &resolvedAgain); err != nil {
		return err
	}
	if resolvedAgain.ResolvedAt == nil || !resolvedAgain.ResolvedAt.Equal(*resolved.ResolvedAt) {
		return fmt.Errorf("second resolve changed resolved_at")
	}
	var openThread struct {
		Items []proofCommentView `json:"items"`
	}
	if err := c.do(http.MethodGet, roundPath(first.Round.ID)+"/comments", c.token, nil, &openThread); err != nil {
		return err
	}
	if len(openThread.Items) != 0 {
		return fmt.Errorf("open comment list has %d items after resolving the thread", len(openThread.Items))
	}
	var fullThread struct {
		Items []proofCommentView `json:"items"`
	}
	if err := c.do(http.MethodGet, roundPath(first.Round.ID)+"/comments?include_resolved=true", c.token, nil, &fullThread); err != nil {
		return err
	}
	if len(fullThread.Items) != 2 || fullThread.Items[1].ResolvedAt == nil {
		return fmt.Errorf("resolved thread = %d items, want 2 with the reply resolved too", len(fullThread.Items))
	}
	fmt.Println("  bad annotation 422, comment idempotent, reply stored, thread resolved as a unit")

	step("proof: customer requests changes")
	var changes proofRoundView
	if err := c.do(http.MethodPost, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/submit",
		proofSession.Session, map[string]any{
			"decision": "changes_requested", "expected_version": reviewVersion,
		}, &changes); err != nil {
		return err
	}
	if changes.Status != "changes_requested" {
		return fmt.Errorf("submit changes = %s, want changes_requested", changes.Status)
	}
	if err := c.expect(http.MethodPost, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/submit",
		proofSession.Session, map[string]any{
			"decision": "changes_requested", "expected_version": reviewVersion,
		}, http.StatusConflict, "version_conflict"); err != nil {
		return err
	}
	var notifications struct {
		Items []struct {
			Type      string    `json:"type"`
			SubjectID uuid.UUID `json:"subject_id"`
		} `json:"items"`
	}
	if err := c.do(http.MethodGet, "/api/v1/notifications?type=proof_changes_requested&limit=100", c.token, nil, &notifications); err != nil {
		return err
	}
	notified := false
	for _, item := range notifications.Items {
		if item.SubjectID == first.Round.ID && item.Type == "proof_changes_requested" {
			notified = true
		}
	}
	if !notified {
		return fmt.Errorf("proof_changes_requested notification for %s missing", first.Round.ID)
	}
	fmt.Println("  changes recorded, stale version 409, staff notification queued")

	step("proof: retouch task and revision uploads")
	var retouch proofRetouchView
	if err := c.do(http.MethodPost, roundPath(first.Round.ID)+"/retouch", c.token, nil, &retouch); err != nil {
		return err
	}
	if retouch.ID == uuid.Nil || retouch.Status != "in_progress" || retouch.Provider != "manual" {
		return fmt.Errorf("retouch task = %+v, want in_progress/manual", retouch)
	}
	if err := c.expect(http.MethodPost, roundPath(first.Round.ID)+"/retouch", c.token, nil,
		http.StatusConflict, "invalid_state"); err != nil {
		return err
	}

	revisionA, err := galleryFixture(900, 650, 0.9)
	if err != nil {
		return err
	}
	revisionB, err := galleryFixture(900, 650, 0.35)
	if err != nil {
		return err
	}
	status, raw, err = uploadProofRevision(c, first.Round.ID, assetIDs[0], "PRF_01.jpg",
		shaHex(revisionB), revisionA, 900, 650)
	if err != nil {
		return err
	}
	if status != http.StatusUnprocessableEntity {
		return fmt.Errorf("mismatched checksum upload = %d: %s", status, string(raw))
	}
	_ = json.Unmarshal(raw, &problem)
	if problem.Code != "checksum_mismatch" {
		return fmt.Errorf("mismatched checksum code = %q, want checksum_mismatch", problem.Code)
	}

	status, raw, err = uploadProofRevision(c, first.Round.ID, assetIDs[0], "PRF_01.jpg",
		shaHex(revisionA), revisionA, 900, 650)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("revision upload = %d: %s", status, string(raw))
	}
	var uploaded importedRevisionView
	if err := json.Unmarshal(raw, &uploaded); err != nil {
		return fmt.Errorf("decode revision: %w", err)
	}
	if uploaded.RevisionNo != 1 || uploaded.Duplicate {
		return fmt.Errorf("first revision = no %d duplicate %v", uploaded.RevisionNo, uploaded.Duplicate)
	}
	status, raw, err = uploadProofRevision(c, first.Round.ID, assetIDs[0], "PRF_01.jpg",
		shaHex(revisionA), revisionA, 900, 650)
	if err != nil {
		return err
	}
	var duplicate importedRevisionView
	if status != http.StatusOK {
		return fmt.Errorf("duplicate revision upload = %d: %s", status, string(raw))
	}
	if err := json.Unmarshal(raw, &duplicate); err != nil {
		return fmt.Errorf("decode duplicate revision: %w", err)
	}
	if !duplicate.Duplicate || duplicate.ID != uploaded.ID {
		return fmt.Errorf("duplicate revision = %s duplicate=%v, want %s", duplicate.ID, duplicate.Duplicate, uploaded.ID)
	}

	if err := c.expect(http.MethodPost, roundPath(first.Round.ID)+"/resubmit", c.token, nil,
		http.StatusConflict, "revisions_incomplete"); err != nil {
		return err
	}
	status, raw, err = uploadProofRevision(c, first.Round.ID, assetIDs[1], "PRF_02.jpg",
		shaHex(revisionB), revisionB, 900, 650)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("second revision upload = %d: %s", status, string(raw))
	}
	fmt.Println("  mismatch 422, first revision created, duplicate 200, early resubmit 409")

	step("proof: revision derivatives and resubmission")
	if err := c.waitProofRevisions(first.Round.ID, 2); err != nil {
		return err
	}
	var resubmitted proofRoundView
	if err := c.do(http.MethodPost, roundPath(first.Round.ID)+"/resubmit", c.token, nil, &resubmitted); err != nil {
		return err
	}
	if resubmitted.Status != "resubmitted" {
		return fmt.Errorf("resubmit status = %s, want resubmitted", resubmitted.Status)
	}
	var refreshed proofManifestPage
	if err := c.do(http.MethodGet, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/manifest?limit=10",
		proofSession.Session, nil, &refreshed); err != nil {
		return err
	}
	for _, item := range refreshed.Items {
		if item.RevisionNo != 1 || item.RevisionID == nil {
			return fmt.Errorf("resubmitted manifest item %s revision = %d (%v), want v1", item.AssetID, item.RevisionNo, item.RevisionID)
		}
		if !strings.Contains(item.DisplayURL, "revisions") {
			return fmt.Errorf("manifest still serves the original preview: %s", item.DisplayURL)
		}
	}
	displayStatus, _, displayBytes, err = c.fetchURL(refreshed.Items[0].DisplayURL)
	if err != nil {
		return err
	}
	if displayStatus != http.StatusOK || len(displayBytes) < 512 {
		return fmt.Errorf("revision display fetch = %d (%d bytes)", displayStatus, len(displayBytes))
	}
	var secondFeedback proofRoundView
	if err := c.do(http.MethodPost, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/submit",
		proofSession.Session, map[string]any{
			"decision": "changes_requested", "expected_version": refreshed.Round.Version,
		}, &secondFeedback); err != nil {
		return err
	}
	if secondFeedback.Status != "changes_requested" {
		return fmt.Errorf("second feedback status = %s", secondFeedback.Status)
	}
	var secondRetouch struct {
		ID     uuid.UUID `json:"id"`
		Status string    `json:"status"`
	}
	if err := c.do(http.MethodPost, roundPath(first.Round.ID)+"/retouch", c.token, nil, &secondRetouch); err != nil {
		return err
	}
	revisionC, err := galleryFixture(900, 650, 0.15)
	if err != nil {
		return err
	}
	revisionD, err := galleryFixture(900, 650, 0.75)
	if err != nil {
		return err
	}
	for i, body := range [][]byte{revisionC, revisionD} {
		status, raw, err := uploadProofRevision(c, first.Round.ID, assetIDs[i], fmt.Sprintf("PRF_%02d.jpg", i+1), shaHex(body), body, 900, 650)
		if err != nil {
			return err
		}
		if status != http.StatusCreated {
			return fmt.Errorf("second-cycle revision upload = %d: %s", status, string(raw))
		}
	}
	if err := c.waitProofRevisions(first.Round.ID, 2); err != nil {
		return err
	}
	var resubmittedAgain proofRoundView
	if err := c.do(http.MethodPost, roundPath(first.Round.ID)+"/resubmit", c.token, nil, &resubmittedAgain); err != nil {
		return err
	}
	if resubmittedAgain.Status != "resubmitted" {
		return fmt.Errorf("second resubmit status = %s", resubmittedAgain.Status)
	}
	fmt.Println("  revisions derived, manifest serves v1 previews, second feedback and retouch cycle succeeded")

	step("proof: customer approval")
	var approved proofRoundView
	if err := c.do(http.MethodPost, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/submit",
		proofSession.Session, map[string]any{
			"decision": "approved", "expected_version": resubmittedAgain.Version,
		}, &approved); err != nil {
		return err
	}
	if approved.Status != "approved" || approved.ApprovedAt == nil {
		return fmt.Errorf("approval = status %s approved_at %v", approved.Status, approved.ApprovedAt)
	}
	var approvedAgain proofRoundView
	if err := c.do(http.MethodPost, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/submit",
		proofSession.Session, map[string]any{
			"decision": "approved", "expected_version": approved.Version,
		}, &approvedAgain); err != nil {
		return err
	}
	if approvedAgain.Status != "approved" {
		return fmt.Errorf("repeated approval = %s, want approved", approvedAgain.Status)
	}
	if err := c.expect(http.MethodPost, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/submit",
		proofSession.Session, map[string]any{
			"decision": "changes_requested", "expected_version": approved.Version,
		}, http.StatusConflict, "invalid_state"); err != nil {
		return err
	}
	notifications.Items = nil
	if err := c.do(http.MethodGet, "/api/v1/notifications?type=proof_approved&limit=100", c.token, nil, &notifications); err != nil {
		return err
	}
	notified = false
	for _, item := range notifications.Items {
		if item.SubjectID == first.Round.ID && item.Type == "proof_approved" {
			notified = true
		}
	}
	if !notified {
		return fmt.Errorf("proof_approved notification for %s missing", first.Round.ID)
	}
	fmt.Println("  approved after two review/retouch cycles; repeat approval idempotent and approval closes feedback")

	step("proof: revocation")
	if err := c.expect(http.MethodDelete, roundPath(first.Round.ID)+"/tokens/"+first.Token.ID.String(),
		c.token, nil, http.StatusNoContent, ""); err != nil {
		return err
	}
	if err := c.expect(http.MethodGet, "/g/v1/proof/session", proofSession.Session, nil,
		http.StatusUnauthorized, "session_revoked"); err != nil {
		return err
	}
	if err := c.expect(http.MethodGet, "/g/v1/proof/rounds/"+first.Round.ID.String()+"/manifest",
		proofSession.Session, nil, http.StatusUnauthorized, "session_revoked"); err != nil {
		return err
	}
	var thirdSession proofSessionView
	if err := c.do(http.MethodPost, "/g/v1/proof/session", "",
		map[string]any{"token": third.Token.Token}, &thirdSession); err != nil {
		return err
	}
	if err := c.do(http.MethodPost, galleryPath+"/revoke", c.token, nil, &gallery); err != nil {
		return err
	}
	if gallery.Status != "revoked" {
		return fmt.Errorf("gallery revoke left status %s", gallery.Status)
	}
	if err := c.expect(http.MethodGet, "/g/v1/proof/session", thirdSession.Session, nil,
		http.StatusGone, "gallery_revoked"); err != nil {
		return err
	}
	if err := c.expect(http.MethodPost, "/g/v1/proof/session", "",
		map[string]any{"token": second.Token.Token}, http.StatusGone, "gallery_revoked"); err != nil {
		return err
	}
	fmt.Println("  revoked link kills its session; revoked gallery answers 410 for sessions and links")

	if err := verifyProofDelivery(c, cfg, []uuid.UUID{first.Round.ID, second.Round.ID, third.Round.ID}); err != nil {
		return err
	}
	return nil
}

type proofRoundView struct {
	ID             uuid.UUID  `json:"id"`
	ProjectID      uuid.UUID  `json:"project_id"`
	GalleryID      uuid.UUID  `json:"gallery_id"`
	RoundNo        int        `json:"round_no"`
	Status         string     `json:"status"`
	DueAt          time.Time  `json:"due_at"`
	ViewedAt       *time.Time `json:"viewed_at,omitempty"`
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	ApprovedAt     *time.Time `json:"approved_at,omitempty"`
	ChangeOrderID  *uuid.UUID `json:"change_order_id,omitempty"`
	OverrideBy     *uuid.UUID `json:"override_by,omitempty"`
	OverrideReason string     `json:"override_reason,omitempty"`
	ClientEmail    string     `json:"client_email,omitempty"`
	Version        int        `json:"version"`
}

type proofTokenView struct {
	ID        uuid.UUID  `json:"id"`
	Token     string     `json:"token"`
	ProofURL  string     `json:"proof_url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type createdRoundView struct {
	Round proofRoundView `json:"round"`
	Token proofTokenView `json:"token"`
	Items int            `json:"items"`
}

type proofCustomerRound struct {
	ID           uuid.UUID `json:"id"`
	ProjectID    uuid.UUID `json:"project_id"`
	GalleryID    uuid.UUID `json:"gallery_id"`
	RoundNo      int       `json:"round_no"`
	Status       string    `json:"status"`
	DueAt        time.Time `json:"due_at"`
	Version      int       `json:"version"`
	ProjectTitle string    `json:"project_title"`
	GalleryTitle string    `json:"gallery_title"`
}

type proofSessionView struct {
	Session   string             `json:"session"`
	ExpiresAt time.Time          `json:"expires_at"`
	Round     proofCustomerRound `json:"round"`
}

type proofManifestPage struct {
	Round      proofCustomerRound  `json:"round"`
	Items      []proofManifestItem `json:"items"`
	Total      int                 `json:"total"`
	Limit      int                 `json:"limit"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

type proofManifestItem struct {
	AssetID    uuid.UUID  `json:"asset_id"`
	Filename   string     `json:"filename"`
	Position   int        `json:"position"`
	Width      int        `json:"width"`
	Height     int        `json:"height"`
	ThumbURL   string     `json:"thumb_url"`
	DisplayURL string     `json:"display_url"`
	RevisionID *uuid.UUID `json:"revision_id,omitempty"`
	RevisionNo int        `json:"revision_no"`
	Comments   int        `json:"comments"`
}

type proofCommentView struct {
	ID         uuid.UUID       `json:"id"`
	ParentID   *uuid.UUID      `json:"parent_id,omitempty"`
	AuthorType string          `json:"author_type"`
	Body       string          `json:"body"`
	Annotation json.RawMessage `json:"annotation,omitempty"`
	ResolvedAt *time.Time      `json:"resolved_at,omitempty"`
	ResolvedBy *uuid.UUID      `json:"resolved_by,omitempty"`
}

type proofRoundItemView struct {
	AssetID       uuid.UUID `json:"asset_id"`
	Filename      string    `json:"filename"`
	RevisionNo    int       `json:"revision_no"`
	RevisionState string    `json:"revision_state,omitempty"`
	ThumbURL      string    `json:"thumb_url,omitempty"`
	DisplayURL    string    `json:"display_url,omitempty"`
}

type proofRetouchView struct {
	ID        uuid.UUID  `json:"id"`
	Status    string     `json:"status"`
	Provider  string     `json:"provider"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

type proofRoundDetail struct {
	proofRoundView
	AssetCount   int                  `json:"asset_count"`
	CommentCount int                  `json:"comment_count"`
	OpenComments int                  `json:"open_comments"`
	Items        []proofRoundItemView `json:"items"`
	Retouch      *proofRetouchView    `json:"retouch,omitempty"`
}

type importedRevisionView struct {
	ID         uuid.UUID `json:"id"`
	RevisionNo int       `json:"revision_no"`
	Status     string    `json:"status"`
	Duplicate  bool      `json:"duplicate"`
}

type proofProblem struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func shaHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// uploadProofRevision posts one multipart retouched file with its declared
// checksum, mirroring the admin upload.
func uploadProofRevision(c *client, roundID, assetID uuid.UUID, filename, declaredSHA string,
	content []byte, width, height int) (int, []byte, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, field := range [][2]string{
		{"asset_id", assetID.String()},
		{"sha256", declaredSHA},
		{"size", strconv.Itoa(len(content))},
		{"width", strconv.Itoa(width)},
		{"height", strconv.Itoa(height)},
		{"note", "smoke revision"},
	} {
		if err := form.WriteField(field[0], field[1]); err != nil {
			return 0, nil, err
		}
	}
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		return 0, nil, err
	}
	if _, err := part.Write(content); err != nil {
		return 0, nil, err
	}
	if err := form.Close(); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost,
		c.base+"/api/v1/proof-rounds/"+roundID.String()+"/revisions", &body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
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

// waitProofRevisions polls the staff view until every snapshot item reports a
// derivative-ready revision from the worker.
func (c *client) waitProofRevisions(roundID uuid.UUID, want int) error {
	deadline := time.Now().Add(c.jobTimeout)
	for {
		var detail proofRoundDetail
		if err := c.do(http.MethodGet, "/api/v1/proof-rounds/"+roundID.String(), c.token, nil, &detail); err != nil {
			return err
		}
		ready := 0
		for _, item := range detail.Items {
			if item.RevisionNo > 0 && item.RevisionState == "derivative_ready" {
				ready++
			}
		}
		if ready == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d revisions still lack derivatives after %s (is the worker running?)",
				want-ready, want, c.jobTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// verifyProofDelivery checks the two notification guarantees that only make
// sense with the real worker: SMTP mail carries the fragment link and the queue
// clears the body once a message is sent. Both checks are opt-in because a
// developer run may have neither mailsink nor database access.
func verifyProofDelivery(c *client, cfg smokeConfig, roundIDs []uuid.UUID) error {
	if cfg.MailDir == "" && cfg.DatabaseURL == "" {
		fmt.Println("  delivery checks skipped (pass -mail-dir and -database-url to inspect SMTP and the queue)")
		return nil
	}
	if cfg.MailDir != "" {
		step("proof: captured customer email")
		if err := waitCapturedProofMail(cfg.MailDir, "/#/p/", c.jobTimeout); err != nil {
			return err
		}
		dirs, err := capturedProofBodies(cfg.MailDir)
		if err != nil {
			return err
		}
		for _, body := range dirs {
			if !strings.Contains(body, "/#/p/") {
				return fmt.Errorf("captured mail without a proof link: %q", firstLine(body))
			}
		}
		fmt.Printf("  %d captured message(s) carry a /#/p/ proof link\n", len(dirs))
	}
	if cfg.DatabaseURL == "" {
		return nil
	}
	step("proof: queue and audit assertions")
	ctx, cancel := context.WithTimeout(context.Background(), c.jobTimeout+30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database for proof checks: %w", err)
	}
	defer pool.Close()

	deadline := time.Now().Add(c.jobTimeout)
	for {
		var sent int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM email_messages
			WHERE related_type = 'proof_round' AND related_id = ANY($1) AND status = 'sent'`,
			roundIDs).Scan(&sent); err != nil {
			return fmt.Errorf("count sent proof mail: %w", err)
		}
		if sent > 0 {
			break
		}
		if time.Now().After(deadline) {
			var summary string
			_ = pool.QueryRow(ctx, `
				SELECT coalesce(string_agg(status || '(' || attempts || ')', ', ' ORDER BY created_at), '')
				FROM email_messages WHERE related_type = 'proof_round' AND related_id = ANY($1)`,
				roundIDs).Scan(&summary)
			return fmt.Errorf("no proof email was sent after %s (queue: %s); check SMTP config and the worker", c.jobTimeout, summary)
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
		WHERE related_type = 'proof_round' AND related_id = ANY($1)
		  AND status = 'sent' AND body_text <> ''`, roundIDs).Scan(&leaked); err != nil {
		return fmt.Errorf("check cleared bodies: %w", err)
	}
	if leaked > 0 {
		return fmt.Errorf("%d sent proof emails kept their body; the link should be cleared after delivery", leaked)
	}
	fmt.Println("  sent proof mail bodies are empty after delivery")
	return nil
}

// waitCapturedProofMail waits for the mailsink to capture a message whose
// decoded body contains needle.
func waitCapturedProofMail(dir, needle string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		bodies, err := capturedProofBodies(dir)
		if err != nil {
			return err
		}
		for _, body := range bodies {
			if strings.Contains(body, needle) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mailsink captured %d message(s) in %s, none with %q after %s", len(bodies), dir, needle, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// capturedProofBodies decodes the base64 bodies mailsink writes to disk. Only
// the body is returned; headers stay out of logs.
func capturedProofBodies(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mailsink dir: %w", err)
	}
	out := []string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".eml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read captured mail: %w", err)
		}
		text := string(raw)
		split := strings.Index(text, "\r\n\r\n")
		if split < 0 {
			continue
		}
		encoded := strings.ReplaceAll(text[split+4:], "\r\n", "")
		encoded = strings.ReplaceAll(encoded, "\n", "")
		body, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			return nil, fmt.Errorf("decode captured mail %s: %w", entry.Name(), err)
		}
		out = append(out, string(body))
	}
	return out, nil
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
