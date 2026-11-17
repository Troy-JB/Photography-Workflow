package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"sync"
	"time"
)

type gallerySessionView struct {
	Session string `json:"session"`
	Album   struct {
		SelectionVersion int `json:"selection_version"`
	} `json:"album"`
}

func runSelectionWrite(ctx context.Context, client *apiClient, token string, cfg config) (*report, error) {
	rep := newReport(cfg)
	seedStart := time.Now()
	assetID, sessions, err := seedSelectionGallery(ctx, client, token, cfg)
	if err != nil {
		return nil, fmt.Errorf("seed selection gallery: %w", err)
	}
	rep.Details["setup_seconds"] = fmt.Sprintf("%.1f", time.Since(seedStart).Seconds())
	rep.Details["sessions"] = len(sessions)

	m := newMetrics()
	started := time.Now()
	deadline := started.Add(cfg.Duration)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			session := sessions[worker%len(sessions)]
			for time.Now().Before(deadline) {
				if err := ctx.Err(); err != nil {
					return
				}
				writeIteration(ctx, client, session, assetID, m)
				time.Sleep(cfg.Think)
			}
		}(i)
	}
	wg.Wait()
	rep.finish(m, started, cfg)
	if rep.Success == 0 {
		rep.fail("no selection write succeeded")
		rep.Passed = false
	}
	return rep, nil
}

// writeIteration models the customer flow: read the current selection version,
// then submit against it. Concurrent writers on the same gallery collide by
// design; a version_conflict is expected contention, not an error.
func writeIteration(ctx context.Context, client *apiClient, session, assetID string, m *metrics) {
	begin := time.Now()
	status, raw, err := client.do(ctx, http.MethodGet, "/g/v1/session", session, nil)
	elapsed := time.Since(begin)
	switch {
	case err != nil:
		m.record("GET /g/v1/session", elapsed, "error")
		return
	case status == http.StatusTooManyRequests:
		m.record("GET /g/v1/session", elapsed, "rate_limited")
		return
	case status != http.StatusOK:
		m.record("GET /g/v1/session", elapsed, "error")
		return
	}
	m.record("GET /g/v1/session", elapsed, "success")

	var view gallerySessionView
	if err := json.Unmarshal(raw, &view); err != nil {
		m.record("GET /g/v1/session", 0, "error")
		return
	}

	body := map[string]any{
		"expected_version": view.Album.SelectionVersion,
		"items":            []map[string]any{{"asset_id": assetID, "decision": "selected"}},
	}
	begin = time.Now()
	status, raw, err = client.do(ctx, http.MethodPost, "/g/v1/selections", session, body)
	elapsed = time.Since(begin)
	switch {
	case err != nil:
		m.record("POST /g/v1/selections", elapsed, "error")
	case status == http.StatusOK:
		m.record("POST /g/v1/selections", elapsed, "success")
	case status == http.StatusConflict && problemCode(raw) == "version_conflict":
		m.record("POST /g/v1/selections", elapsed, "conflict")
	case status == http.StatusTooManyRequests:
		m.record("POST /g/v1/selections", elapsed, "rate_limited")
	default:
		m.record("POST /g/v1/selections", elapsed, "error")
	}
}

// seedSelectionGallery builds one published gallery with a single verified
// asset and exchanges a share token for every writer session. Each session has
// its own 120/min bucket, so the write load does not collapse into 429s.
func seedSelectionGallery(ctx context.Context, client *apiClient, token string, cfg config) (string, []string, error) {
	tag := fmt.Sprintf("LT%d", time.Now().Unix()%1_000_000)
	var project struct {
		ID string `json:"id"`
	}
	if err := client.json(ctx, http.MethodPost, "/api/v1/projects", token,
		map[string]any{"title": "Loadtest selection " + tag}, &project); err != nil {
		return "", nil, err
	}

	body, err := loadFixtureJPEG(800, 600)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(body)

	var batch struct {
		ID string `json:"id"`
	}
	if err := client.json(ctx, http.MethodPost, "/agent/v1/ingest-batches", token,
		map[string]any{"project_id": project.ID, "device_id": "loadtest"}, &batch); err != nil {
		return "", nil, err
	}
	var registered struct {
		Assets []struct {
			AssetID string `json:"asset_id"`
		} `json:"assets"`
	}
	if err := client.json(ctx, http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID+"/assets/batch", token,
		map[string]any{"files": []map[string]any{{
			"rel_path": "loadtest/LT_0001.JPG",
			"size":     len(body),
			"sha256":   hex.EncodeToString(sum[:]),
			"mime":     "image/jpeg",
			"width":    800,
			"height":   600,
		}}}, &registered); err != nil {
		return "", nil, err
	}
	if len(registered.Assets) != 1 {
		return "", nil, fmt.Errorf("register returned %d assets, want 1", len(registered.Assets))
	}
	assetID := registered.Assets[0].AssetID

	var plan uploadPlan
	if err := client.json(ctx, http.MethodPost, "/agent/v1/uploads/presign", token,
		map[string]any{"asset_id": assetID, "size": len(body)}, &plan); err != nil {
		return "", nil, err
	}
	if plan.Mode != "single" {
		return "", nil, fmt.Errorf("presign mode %q, want single", plan.Mode)
	}
	if err := putObject(ctx, client, plan.URL, body); err != nil {
		return "", nil, err
	}
	if err := client.json(ctx, http.MethodPost, "/agent/v1/uploads/"+assetID+"/complete", token,
		map[string]any{"upload_id": plan.UploadID, "parts": []any{}}, nil); err != nil {
		return "", nil, err
	}
	var finalized struct {
		ID string `json:"id"`
	}
	if err := client.json(ctx, http.MethodPost, "/agent/v1/ingest-batches/"+batch.ID+"/finalize", token,
		nil, &finalized); err != nil {
		return "", nil, err
	}
	if err := client.waitJob(ctx, token, finalized.ID, cfg.JobTimeout); err != nil {
		return "", nil, err
	}

	var gallery struct {
		ID string `json:"id"`
	}
	if err := client.json(ctx, http.MethodPost, "/api/v1/galleries", token, map[string]any{
		"project_id": project.ID,
		"title":      "Loadtest gallery " + tag,
		"expires_at": time.Now().Add(2 * time.Hour).Format(time.RFC3339),
	}, &gallery); err != nil {
		return "", nil, err
	}
	if err := client.json(ctx, http.MethodPost, "/api/v1/galleries/"+gallery.ID+"/assets", token,
		map[string]any{"asset_ids": []string{assetID}}, nil); err != nil {
		return "", nil, err
	}

	if err := client.json(ctx, http.MethodPost, "/api/v1/projects/"+project.ID+"/derivatives:derive", token,
		map[string]any{"asset_ids": []string{}, "force": false}, nil); err != nil {
		return "", nil, err
	}
	if err := waitDerivatives(ctx, client, token, project.ID, assetID, cfg.JobTimeout); err != nil {
		return "", nil, err
	}
	if err := publishGallery(ctx, client, token, gallery.ID, cfg.JobTimeout); err != nil {
		return "", nil, err
	}

	shares := cfg.Concurrency
	if shares > 25 {
		// The session exchange is limited to 30/min per IP; stay well below it.
		shares = 25
	}
	sessions := make([]string, 0, shares)
	for i := 0; i < shares; i++ {
		var issued struct {
			Token string `json:"token"`
		}
		if err := client.json(ctx, http.MethodPost, "/api/v1/galleries/"+gallery.ID+"/tokens", token,
			map[string]any{"label": fmt.Sprintf("loadtest-%d", i), "max_visits": 1_000_000}, &issued); err != nil {
			return "", nil, err
		}
		var view gallerySessionView
		if err := client.json(ctx, http.MethodPost, "/g/v1/session", "",
			map[string]any{"token": issued.Token}, &view); err != nil {
			return "", nil, err
		}
		sessions = append(sessions, view.Session)
		time.Sleep(50 * time.Millisecond)
	}
	return assetID, sessions, nil
}

func putObject(ctx context.Context, client *apiClient, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "image/jpeg")
	resp, err := client.hc.Do(req)
	if err != nil {
		return fmt.Errorf("upload fixture: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("upload fixture: status %d", resp.StatusCode)
	}
	return nil
}

func waitDerivatives(ctx context.Context, client *apiClient, token, projectID, assetID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var listed struct {
			Items []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"items"`
		}
		if err := client.json(ctx, http.MethodGet, "/api/v1/projects/"+projectID+"/assets?limit=200",
			token, nil, &listed); err != nil {
			return err
		}
		for _, item := range listed.Items {
			if item.ID == assetID && item.Status == "derivative_ready" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("asset %s still lacks derivatives after %s (is the worker running?)", assetID, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func publishGallery(ctx context.Context, client *apiClient, token, galleryID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, raw, err := client.do(ctx, http.MethodPost, "/api/v1/galleries/"+galleryID+"/publish", token, nil)
		switch {
		case err != nil:
			return err
		case status == http.StatusOK:
			return nil
		case status == http.StatusConflict && problemCode(raw) == "derivatives_incomplete":
			// The worker may still be stamping watermarked previews.
		default:
			return fmt.Errorf("publish gallery: status %d: %s", status, truncate(raw, 300))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gallery still reports derivatives_incomplete after %s", timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func loadFixtureJPEG(w, h int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			fy := float64(y) / float64(h)
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(255 * (0.25 + 0.65*fx)),
				G: uint8(255 * (0.2 + 0.7*fy)),
				B: uint8(255 * (0.35 + 0.45*(1-fx))),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
