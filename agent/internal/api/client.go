// Package api is the agent's HTTP client for the photography workflow server.
// It covers authentication, device registration, ingest batches, uploads and
// job polling. Every mutating call takes an idempotency key: retries (including
// ones that cross a crash/restart) replay the same server-side operation
// instead of creating a second batch, asset or verification job.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Version is stamped into the User-Agent and device registrations. Release
// builds override it with -ldflags.
var Version = "0.4.0-dev"

// Error is a server problem response (RFC 7807 style) or a transport-level
// failure with a status attached.
type Error struct {
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id"`
}

func (e *Error) Error() string {
	msg := e.Code
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s", msg, e.Detail)
	}
	return msg
}

// Permanent reports whether retrying the exact same request cannot help.
// Authentication failures are handled by the token manager before this is
// ever consulted for a retry decision.
func (e *Error) Permanent() bool {
	switch {
	case e.Status == http.StatusTooManyRequests,
		e.Status == http.StatusRequestTimeout,
		e.Status >= 500:
		return false
	}
	return e.Status >= 400 && e.Status < 500
}

// Is reports whether err is an *Error with the given status and code.
func Is(err error, status int, code string) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == status && (code == "" || e.Code == code)
}

func IsUnauthorized(err error) bool {
	return Is(err, http.StatusUnauthorized, "")
}

// IsConflict reports a 409 regardless of the problem code.
func IsConflict(err error) bool { return Is(err, http.StatusConflict, "") }

// RetryPolicy controls retries for idempotent requests. Idempotency keys make
// replay safe for mutations; GETs are naturally safe.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      float64
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxAttempts: 5, BaseDelay: time.Second, MaxDelay: 60 * time.Second, Jitter: 0.2}
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 5
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = time.Second
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = 60 * time.Second
	}
	if p.Jitter < 0 || p.Jitter > 1 {
		p.Jitter = 0.2
	}
	return p
}

func (p RetryPolicy) delay(attempt int) time.Duration {
	d := p.BaseDelay << (attempt - 1)
	if d > p.MaxDelay || d <= 0 {
		d = p.MaxDelay
	}
	if p.Jitter > 0 {
		// jitter in [1-j, 1+j)
		f := 1 - p.Jitter + 2*p.Jitter*rand.Float64()
		d = time.Duration(float64(d) * f)
	}
	return d
}

// Client talks to one server. Token is optional: login and refresh use an
// unauthenticated path, everything else expects a token supplier.
type Client struct {
	base  *url.URL
	http  *http.Client
	token func(ctx context.Context) (string, error)
	retry RetryPolicy

	// upload client talks to the storage service through presigned URLs. It
	// never carries the API token.
	upload         *http.Client
	bandwidth      *Bandwidth
	onUnauthorized func(ctx context.Context) error
}

// Options configure a Client.
type Options struct {
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
	Retry RetryPolicy
	// Bandwidth limits upload throughput; nil or zero means unlimited.
	Bandwidth *Bandwidth
	// OnUnauthorized forces a token refresh after a 401 so the request can be
	// retried once with fresh credentials.
	OnUnauthorized func(ctx context.Context) error
}

// New validates the server URL and returns a client. Plain HTTP is accepted
// only for loopback development servers: credentials and tokens must never
// cross a network in the clear.
func New(baseURL string, opts Options) (*Client, error) {
	u, err := ParseServerURL(baseURL)
	if err != nil {
		return nil, err
	}
	hc := opts.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	upload := &http.Client{Transport: uploadTransport()} // large bodies; the context bounds them
	return &Client{
		base:           u,
		http:           hc,
		token:          opts.Token,
		retry:          opts.Retry.normalized(),
		upload:         upload,
		bandwidth:      opts.Bandwidth,
		onUnauthorized: opts.OnUnauthorized,
	}, nil
}

// ParseServerURL enforces the HTTPS rule and strips a trailing slash.
func ParseServerURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("服务器地址为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("无效的服务器地址：%w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("服务器地址必须使用 https://（http:// 仅限本机调试）")
	}
	if u.Host == "" {
		return nil, errors.New("服务器地址缺少主机名")
	}
	if u.User != nil {
		return nil, errors.New("服务器地址不能包含用户名或密码")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, errors.New("远程服务器必须使用 https://")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BaseURL returns the normalized server root without a trailing slash.
func (c *Client) BaseURL() string { return c.base.String() }

func (c *Client) resolve(path string) string {
	return c.base.String() + path
}

func (c *Client) userAgent() string {
	return "PWF-Agent/" + Version
}

// doJSON performs an authenticated request with retries. body is marshalled
// once so each retry sends identical bytes (and therefore the same
// idempotency hash).
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any,
	idemKey string, authenticated bool) error {
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = data
	}
	var attempt int
	authRetried := false
	for {
		attempt++
		req, err := http.NewRequestWithContext(ctx, method, c.resolve(path), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent())
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idemKey != "" {
			req.Header.Set("Idempotency-Key", idemKey)
		}
		if authenticated && c.token != nil {
			tok, err := c.token(ctx)
			if err != nil {
				return err
			}
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt >= c.retry.MaxAttempts {
				return fmt.Errorf("请求失败（%s %s）：%w", method, path, err)
			}
			if err := sleepCtx(ctx, c.retry.delay(attempt)); err != nil {
				return err
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			if attempt >= c.retry.MaxAttempts {
				return fmt.Errorf("读取响应失败（%s %s）：%w", method, path, readErr)
			}
			if err := sleepCtx(ctx, c.retry.delay(attempt)); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(data) == 0 {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("解析响应失败（%s %s）：%w", method, path, err)
			}
			return nil
		}
		apiErr := decodeProblem(resp.StatusCode, data)
		if resp.StatusCode == http.StatusUnauthorized && authenticated &&
			!authRetried && c.onUnauthorized != nil {
			// A rotated or revoked access token is recoverable exactly once.
			authRetried = true
			attempt--
			if err := c.onUnauthorized(ctx); err != nil {
				return err
			}
			continue
		}
		if apiErr.Permanent() || attempt >= c.retry.MaxAttempts {
			return apiErr
		}
		if err := sleepCtx(ctx, c.retry.delay(attempt)); err != nil {
			return err
		}
	}
}

func decodeProblem(status int, data []byte) *Error {
	e := &Error{Status: status}
	var p struct {
		Code      string `json:"code"`
		Title     string `json:"title"`
		Detail    string `json:"detail"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(data, &p); err == nil {
		e.Code, e.Title, e.Detail, e.RequestID = p.Code, p.Title, p.Detail, p.RequestID
	}
	if e.Title == "" && e.Code == "" {
		e.Title = strings.TrimSpace(string(data))
		if len(e.Title) > 300 {
			e.Title = e.Title[:300]
		}
	}
	return e
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
