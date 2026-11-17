package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota + 1
	ctxTraceID
)

func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

func TraceIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxTraceID).(string); ok {
		return v
	}
	return ""
}

func WithTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := traceIDFromHeader(r.Header.Get("traceparent"))
		if traceID == "" {
			raw := make([]byte, 16)
			_, _ = rand.Read(raw)
			traceID = hex.EncodeToString(raw)
		}
		w.Header().Set("X-Trace-Id", traceID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxTraceID, traceID)))
	})
}

// traceIDFromHeader accepts W3C traceparent: 00-<32 hex trace>-<16 hex span>-<flags>.
func traceIDFromHeader(header string) string {
	parts := strings.Split(header, "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return ""
	}
	return strings.ToLower(parts[1])
}

func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			log.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"bytes", sw.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFrom(r.Context()),
				"trace_id", TraceIDFrom(r.Context()),
				"ip", r.RemoteAddr,
			)
		})
	}
}

func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic", "err", rec, "stack", string(debug.Stack()), "path", r.URL.Path)
					WriteProblem(w, r, http.StatusInternalServerError, "internal_error", "Internal error", "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Common installs the middleware chain shared by all routes.
func Common(log *slog.Logger, requestTimeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return WithRequestID(WithTrace(Logger(log)(Recoverer(log)(Timeout(requestTimeout)(
			middleware.RequestSize(8 << 20)(middleware.RealIP(next)))))))
	}
}

// CORS permits browser clients hosted on explicitly configured origins. Empty
// configuration leaves same-origin and reverse-proxy deployments unchanged.
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := allowed[origin]; !ok {
				if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
					http.Error(w, "origin not allowed", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Add("Vary", "Access-Control-Request-Method")
				w.Header().Add("Vary", "Access-Control-Request-Headers")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, Idempotency-Key, Last-Event-ID, Traceparent, X-Request-Id")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds handler work with a request context deadline and answers 504
// when the deadline wins. Writes after the deadline are dropped.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d <= 0 || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			tw := &timeoutWriter{ResponseWriter: w}
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					if ctx.Err() == context.DeadlineExceeded {
						tw.timeout(r)
					}
				case <-done:
				}
			}()
			next.ServeHTTP(tw, r.WithContext(ctx))
			close(done)
		})
	}
}

type timeoutWriter struct {
	http.ResponseWriter
	mu       sync.Mutex
	wrote    bool
	timedOut bool
}

func (w *timeoutWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wrote || w.timedOut {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *timeoutWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut {
		return len(b), nil
	}
	if !w.wrote {
		w.wrote = true
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *timeoutWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timedOut {
		return
	}
	if !w.wrote {
		w.wrote = true
		w.ResponseWriter.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *timeoutWriter) timeout(r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timedOut = true
	if w.wrote {
		return
	}
	p := Problem{
		Type:      "https://pwf.local/errors/timeout",
		Title:     "Request timed out",
		Status:    http.StatusGatewayTimeout,
		Code:      "timeout",
		RequestID: RequestIDFrom(r.Context()),
	}
	w.ResponseWriter.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.ResponseWriter.WriteHeader(http.StatusGatewayTimeout)
	_ = json.NewEncoder(w.ResponseWriter).Encode(p)
}
