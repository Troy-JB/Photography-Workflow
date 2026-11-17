package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ---------- auth ----------

// Login exchanges credentials for a session. It deliberately bypasses the
// token supplier.
func (c *Client) Login(ctx context.Context, email, password, tenantSlug string) (*Session, error) {
	var out Session
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/login",
		LoginRequest{Email: email, Password: password, TenantSlug: tenantSlug}, &out, "", false)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Refresh rotates a refresh token. The old token is invalid after success.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	var out Session
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/auth/refresh",
		RefreshRequest{RefreshToken: refreshToken}, &out, "", false)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Logout(ctx context.Context, refreshToken string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/auth/logout",
		RefreshRequest{RefreshToken: refreshToken}, nil, "", false)
}

func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	var out Profile
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/auth/me", nil, &out, "", true); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- projects ----------

// ListProjects returns one page of projects. The import screen uses it to
// attach an import to a server project.
func (c *Client) ListProjects(ctx context.Context, limit int) ([]Project, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	var out ProjectList
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects?"+q.Encode(), nil, &out, "", true); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []Project{}
	}
	return out.Items, nil
}

// ---------- devices ----------

func (c *Client) RegisterDevice(ctx context.Context, req DeviceRegistrationRequest) (*Device, error) {
	var out Device
	if err := c.doJSON(ctx, http.MethodPost, "/agent/v1/devices", req, &out, "", true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	var out HeartbeatResponse
	if err := c.doJSON(ctx, http.MethodPost, "/agent/v1/heartbeats", req, &out, "", true); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- ingest ----------

func (c *Client) CreateBatch(ctx context.Context, idemKey string, req CreateBatchRequest) (*Batch, error) {
	var out Batch
	if err := c.doJSON(ctx, http.MethodPost, "/agent/v1/ingest-batches", req, &out, idemKey, true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RegisterFiles(ctx context.Context, batchID, idemKey string, files []FileInput) (*RegisterResult, error) {
	var out RegisterResult
	path := fmt.Sprintf("/agent/v1/ingest-batches/%s/assets/batch", url.PathEscape(batchID))
	if err := c.doJSON(ctx, http.MethodPost, path, map[string]any{"files": files}, &out, idemKey, true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Presign(ctx context.Context, idemKey string, req PresignRequest) (*UploadPlan, error) {
	var out UploadPlan
	if err := c.doJSON(ctx, http.MethodPost, "/agent/v1/uploads/presign", req, &out, idemKey, true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CompleteUpload(ctx context.Context, assetID, idemKey string, req CompleteUploadRequest) (*Asset, error) {
	var out Asset
	path := fmt.Sprintf("/agent/v1/uploads/%s/complete", url.PathEscape(assetID))
	if err := c.doJSON(ctx, http.MethodPost, path, req, &out, idemKey, true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) FinalizeBatch(ctx context.Context, batchID, idemKey string) (*Job, error) {
	var out Job
	path := fmt.Sprintf("/agent/v1/ingest-batches/%s/finalize", url.PathEscape(batchID))
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &out, idemKey, true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Job(ctx context.Context, jobID string) (*Job, error) {
	var out Job
	path := fmt.Sprintf("/agent/v1/jobs/%s", url.PathEscape(jobID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out, "", true); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) BatchStatus(ctx context.Context, batchID string) (*BatchStatus, error) {
	var out BatchStatus
	path := fmt.Sprintf("/agent/v1/ingest-batches/%s", url.PathEscape(batchID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out, "", true); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- derivatives ----------

// PresignDerivative signs a single PUT for an agent-rendered gallery preview.
func (c *Client) PresignDerivative(ctx context.Context, idemKey string, req DerivativePresignRequest) (*DerivativePresign, error) {
	var out DerivativePresign
	if err := c.doJSON(ctx, http.MethodPost, "/agent/v1/derivatives:presign", req, &out, idemKey, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// CompleteDerivative reports the uploaded preview. The server re-hashes the
// object, records the derivative and queues the thumb/display rendering.
func (c *Client) CompleteDerivative(ctx context.Context, assetID, idemKey string, req DerivativeCompleteRequest) (*Derivative, error) {
	var out Derivative
	path := fmt.Sprintf("/agent/v1/derivatives/%s:complete", url.PathEscape(assetID))
	if err := c.doJSON(ctx, http.MethodPost, path, req, &out, idemKey, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAssets pages the project's assets. Verification only reports aggregate
// counts, so the agent matches its own asset ids to learn which files verified
// and which turned up missing or corrupt.
func (c *Client) ListAssets(ctx context.Context, projectID string, limit, offset int) ([]Asset, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	var out AssetList
	path := fmt.Sprintf("/api/v1/projects/%s/assets?%s", url.PathEscape(projectID), q.Encode())
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out, "", true); err != nil {
		return nil, 0, err
	}
	if out.Items == nil {
		out.Items = []Asset{}
	}
	return out.Items, out.Total, nil
}
