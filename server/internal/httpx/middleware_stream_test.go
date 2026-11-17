package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (w *flushRecorder) Flush() { w.flushed = true }

func TestStatusWriterFlushForwards(t *testing.T) {
	underlying := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &statusWriter{ResponseWriter: underlying}
	w.Flush()
	if !underlying.flushed || w.status != http.StatusOK {
		t.Fatalf("flush forwarded=%v status=%d", underlying.flushed, w.status)
	}
}

func TestTimeoutLeavesEventStreamsOpen(t *testing.T) {
	handler := Timeout(time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("event: refresh\n\n"))
	}))
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Body.String() != "event: refresh\n\n" {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
}
