package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type apiClient struct {
	base string
	hc   *http.Client
}

func newAPIClient(base string, timeout time.Duration) *apiClient {
	return &apiClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: timeout}}
}

func (c *apiClient) do(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func (c *apiClient) json(ctx context.Context, method, path, token string, body, out any) error {
	status, raw, err := c.do(ctx, method, path, token, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, status, truncate(raw, 300))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

func problemCode(raw []byte) string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Code
}

func truncate(raw []byte, limit int) string {
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "…"
}

func (c *apiClient) login(ctx context.Context, email, password string) (string, error) {
	var session struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.json(ctx, http.MethodPost, "/api/v1/auth/login", "",
		map[string]string{"email": email, "password": password}, &session); err != nil {
		return "", err
	}
	if session.AccessToken == "" {
		return "", fmt.Errorf("login returned an empty access token")
	}
	return session.AccessToken, nil
}

type uploadPlan struct {
	Mode      string `json:"mode"`
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	UploadID  string `json:"upload_id"`
}

type ingestJob struct {
	Status    string `json:"status"`
	LastError string `json:"last_error"`
}

func (c *apiClient) waitJob(ctx context.Context, token, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var job ingestJob
		if err := c.json(ctx, http.MethodGet, "/agent/v1/jobs/"+id, token, nil, &job); err != nil {
			return err
		}
		switch job.Status {
		case "succeeded":
			return nil
		case "failed":
			return fmt.Errorf("job %s failed: %s", id, job.LastError)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job %s still %s after %s (is the worker running?)", id, job.Status, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
