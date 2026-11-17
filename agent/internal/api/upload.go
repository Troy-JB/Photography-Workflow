package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// PutObject uploads through a presigned storage URL. open is called once per
// attempt so a retry can send the body again; it must return a fresh reader.
// The returned ETag is normalized (surrounding quotes removed), matching what
// the server's storage layer expects when the multipart upload is completed.
func (c *Client) PutObject(ctx context.Context, rawURL string, size int64,
	open func() (io.ReadCloser, error)) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("无效的对象大小 %d", size)
	}
	if err := ValidatePresignURL(rawURL); err != nil {
		return "", err
	}
	var attempt int
	for {
		attempt++
		body, err := open()
		if err != nil {
			return "", err
		}
		var reader io.Reader = body
		if c.bandwidth != nil {
			reader = c.bandwidth.wrap(ctx, body)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, reader)
		if err != nil {
			body.Close()
			return "", err
		}
		req.ContentLength = size
		req.Header.Set("User-Agent", c.userAgent())
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.upload.Do(req)
		if err != nil {
			body.Close()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if attempt >= c.retry.MaxAttempts {
				return "", fmt.Errorf("上传失败（%d 字节）：%w", size, err)
			}
			if err := sleepCtx(ctx, c.retry.delay(attempt)); err != nil {
				return "", err
			}
			continue
		}
		// Drain a little so the connection can be reused; the body is usually
		// empty on PUT responses.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			etag := normalizeETag(resp.Header.Get("ETag"))
			if etag == "" {
				return "", errors.New("存储服务未返回 ETag，无法续传校验")
			}
			return etag, nil
		case resp.StatusCode == http.StatusForbidden,
			resp.StatusCode == http.StatusBadRequest,
			resp.StatusCode == http.StatusNotFound:
			// Presigned URLs expire; the caller re-presigns with a new epoch.
			return "", &Error{Status: resp.StatusCode, Code: "presign_expired",
				Detail: "预签名地址已失效或对象不存在，需要重新获取上传计划"}
		}
		if attempt >= c.retry.MaxAttempts {
			return "", &Error{Status: resp.StatusCode, Code: "upload_failed",
				Detail: fmt.Sprintf("存储服务返回 %s", resp.Status)}
		}
		if err := sleepCtx(ctx, c.retry.delay(attempt)); err != nil {
			return "", err
		}
	}
}

// ValidatePresignURL enforces HTTPS for storage endpoints. Loopback HTTP is
// allowed so a developer can run MinIO locally; anything else must be TLS.
// The URL comes from an authenticated server response, so the server is the
// trust anchor for the host; the agent still refuses credentials in URLs.
func ValidatePresignURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("无效的预签名地址：%w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("预签名地址必须使用 https://")
	}
	if u.Host == "" {
		return errors.New("预签名地址缺少主机名")
	}
	if u.User != nil {
		return errors.New("预签名地址不能包含用户名或密码")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return errors.New("远程存储必须使用 https://")
	}
	return nil
}

func normalizeETag(etag string) string {
	etag = strings.TrimSpace(etag)
	etag = strings.TrimPrefix(etag, "W/")
	etag = strings.Trim(etag, `"`)
	return etag
}

// ---------- bandwidth ----------

// Bandwidth is a shared token bucket measured in bytes per second. A nil or
// zero bucket means unlimited. It throttles after each read, so bursts are at
// most one read chunk (64 KiB).
type Bandwidth struct {
	bps  int64
	mu   sync.Mutex
	next time.Time
}

// NewBandwidth returns nil for unlimited throughput.
func NewBandwidth(bytesPerSecond int64) *Bandwidth {
	if bytesPerSecond <= 0 {
		return nil
	}
	return &Bandwidth{bps: bytesPerSecond}
}

func (b *Bandwidth) wrap(ctx context.Context, r io.Reader) io.Reader {
	return &throttledReader{ctx: ctx, r: r, b: b}
}

func (b *Bandwidth) limit(n int64) time.Duration {
	if b == nil || b.bps <= 0 || n <= 0 {
		return 0
	}
	return time.Duration(float64(n) * float64(time.Second) / float64(b.bps))
}

func (b *Bandwidth) wait(ctx context.Context, n int64) error {
	d := b.limit(n)
	if d <= 0 {
		return nil
	}
	b.mu.Lock()
	now := time.Now()
	if b.next.Before(now) {
		b.next = now
	}
	b.next = b.next.Add(d)
	wait := b.next.Sub(now)
	b.mu.Unlock()
	return sleepCtx(ctx, wait)
}

const throttleChunk = 64 << 10

type throttledReader struct {
	ctx context.Context
	r   io.Reader
	b   *Bandwidth
}

func (t *throttledReader) Read(p []byte) (int, error) {
	if len(p) > throttleChunk {
		p = p[:throttleChunk]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		if werr := t.b.wait(t.ctx, int64(n)); werr != nil {
			return 0, werr
		}
	}
	return n, err
}

// uploadTransport bounds connection setup without capping the whole body: a
// multi-gigabyte upload on a slow line may legitimately take hours.
func uploadTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	t.TLSHandshakeTimeout = 15 * time.Second
	t.IdleConnTimeout = 90 * time.Second
	t.ExpectContinueTimeout = 5 * time.Second
	return t
}
