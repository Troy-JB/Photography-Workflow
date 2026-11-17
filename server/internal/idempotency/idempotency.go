package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/httpx"
)

const maxBody = 2 << 20

type replay struct {
	Status      int
	Body        []byte
	ContentType string
	Headers     http.Header
}

type stored struct {
	Hash        string
	Status      int
	Body        []byte
	ContentType string
	Headers     http.Header
}

// Middleware honors Idempotency-Key on mutating endpoints. The first request
// reserves (tenant, endpoint, key); concurrent or repeated requests with the
// same key replay the stored response. Reusing a key with a different body is a
// conflict, never a silent overwrite.
func Middleware(pool *pgxpool.Pool, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			if key == "" || !mutating(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			tenantID, ok := auth.TenantFrom(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				httpx.WriteProblem(w, r, http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large", "")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			endpoint := r.Method + " " + routePattern(r)
			// Route templates intentionally share the same idempotency namespace, so
			// the concrete path must also be hashed. Otherwise the same key/body
			// could replay a response from a different resource ID.
			requestHash := sha256.Sum256(append([]byte(endpoint+"\n"+r.URL.EscapedPath()+"\n"), body...))
			hash := hex.EncodeToString(requestHash[:])

			var prior *stored
			claimed, err := reserve(r.Context(), pool, tenantID, endpoint, key, hash)
			if err != nil {
				httpx.Internal(w, r, log, err)
				return
			}
			if !claimed {
				prior, err = load(r.Context(), pool, tenantID, endpoint, key)
				if err != nil {
					httpx.Internal(w, r, log, err)
					return
				}
				switch {
				case prior == nil:
					httpx.WriteProblem(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key conflict", "retry the request")
					return
				case prior.Status == 0:
					httpx.WriteProblem(w, r, http.StatusConflict, "idempotency_in_progress", "Request already in progress", "retry after the first request completes")
					return
				case prior.Hash != hash:
					httpx.WriteProblem(w, r, http.StatusConflict, "idempotency_conflict", "Idempotency key reused", "same key with a different request body")
					return
				default:
					writeReplay(w, &replay{Status: prior.Status, Body: prior.Body, ContentType: prior.ContentType, Headers: prior.Headers})
					return
				}
			}

			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			completed := false
			defer func() {
				if !completed {
					release(context.Background(), pool, log, tenantID, endpoint, key)
				}
			}()
			next.ServeHTTP(rec, r)
			completed = true
			if r.Context().Err() != nil {
				// Timeout middleware may write its response outside this recorder.
				// Never leave a status=0 reservation behind in that case.
				release(context.Background(), pool, log, tenantID, endpoint, key)
				return
			}

			if rec.status >= 500 {
				release(context.Background(), pool, log, tenantID, endpoint, key)
				return
			}
			if err := store(r.Context(), pool, tenantID, endpoint, key, rec.status, rec.body.Bytes(), rec.Header().Get("Content-Type"), replayHeaders(rec.Header())); err != nil {
				log.Warn("store idempotent response", "key", key, "err", err)
			}
		})
	}
}

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return r.URL.Path
}

func reserve(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, endpoint, key, hash string) (bool, error) {
	var claimed bool
	err := db.WithTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (tenant_id, endpoint, key, request_hash, status)
			VALUES ($1,$2,$3,$4,0)
			ON CONFLICT DO NOTHING`, tenantID, endpoint, key, hash)
		if err != nil {
			return fmt.Errorf("reserve idempotency key: %w", err)
		}
		claimed = ct.RowsAffected() == 1
		return nil
	})
	return claimed, err
}

func load(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, endpoint, key string) (*stored, error) {
	var out *stored
	err := db.WithTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		var s stored
		var headers string
		if err := tx.QueryRow(ctx, `
				SELECT request_hash, status, coalesce(response_body::text, ''), response_content_type,
				       coalesce(response_headers::text, '{}')
				FROM idempotency_keys
				WHERE tenant_id = $1 AND endpoint = $2 AND key = $3`,
			tenantID, endpoint, key).Scan(&s.Hash, &s.Status, &s.Body, &s.ContentType, &headers); err != nil {
			if err == pgx.ErrNoRows {
				return nil
			}
			return fmt.Errorf("load idempotency key: %w", err)
		}
		if err := json.Unmarshal([]byte(headers), &s.Headers); err != nil {
			return fmt.Errorf("decode idempotent headers: %w", err)
		}
		out = &s
		return nil
	})
	return out, err
}

func store(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, endpoint, key string, status int, body []byte, contentType string, headers http.Header) error {
	if !json.Valid(body) {
		body = []byte("null")
	}
	if contentType == "" {
		contentType = "application/json"
	}
	headerJSON, err := json.Marshal(headers)
	if err != nil {
		return fmt.Errorf("marshal idempotent headers: %w", err)
	}
	return db.WithTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE idempotency_keys
			SET status = $4, response_body = $5, response_content_type = $6,
				response_headers = $7, updated_at = now()
			WHERE tenant_id = $1 AND endpoint = $2 AND key = $3`,
			tenantID, endpoint, key, status, body, contentType, headerJSON); err != nil {
			return fmt.Errorf("store idempotent response: %w", err)
		}
		return nil
	})
}

func release(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, tenantID uuid.UUID, endpoint, key string) {
	if err := db.WithTenant(ctx, pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys WHERE tenant_id = $1 AND endpoint = $2 AND key = $3 AND status = 0`,
			tenantID, endpoint, key)
		return err
	}); err != nil {
		log.Warn("release idempotency key", "key", key, "err", err)
	}
}

func writeReplay(w http.ResponseWriter, r *replay) {
	for key, values := range r.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if r.ContentType != "" {
		w.Header().Set("Content-Type", r.ContentType)
	}
	w.WriteHeader(r.Status)
	if len(r.Body) > 0 {
		_, _ = w.Write(r.Body)
	}
}

func replayHeaders(src http.Header) http.Header {
	out := make(http.Header)
	for _, key := range []string{"Content-Type", "Location", "Retry-After"} {
		for _, value := range src.Values(key) {
			out.Add(key, value)
		}
	}
	return out
}

type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
