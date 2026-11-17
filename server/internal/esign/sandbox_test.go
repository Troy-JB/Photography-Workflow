package esign

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSecret = "test-secret-at-least-16"

func newTestSandbox(t *testing.T) *Sandbox {
	t.Helper()
	s, err := NewSandbox(testSecret, "https://sign.example.test", 5*time.Minute)
	if err != nil {
		t.Fatalf("new sandbox: %v", err)
	}
	s.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return s
}

func signedRequest(t *testing.T, s *Sandbox, body []byte, at time.Time) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhooks/esign/sandbox", strings.NewReader(string(body)))
	r.Header.Set("X-PWF-Signature", SignRequestBody(testSecret, at, body))
	return r
}

func TestNewSandboxValidation(t *testing.T) {
	if _, err := NewSandbox("short", "https://sign.example.test", time.Minute); err == nil {
		t.Fatal("short secret should be rejected")
	}
	if _, err := NewSandbox(testSecret, "  ", time.Minute); err == nil {
		t.Fatal("missing sign base url should be rejected")
	}
}

func TestCreateSignTask(t *testing.T) {
	s := newTestSandbox(t)
	task, err := s.CreateSignTask(t.Context(), SignTaskRequest{SignerName: "Alice"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if !strings.HasPrefix(task.TaskID, "sbx_") || len(task.TaskID) <= 4 {
		t.Fatalf("task id = %q", task.TaskID)
	}
	if !strings.Contains(task.SignURL, task.TaskID) || !strings.Contains(task.SignURL, "token=") {
		t.Fatalf("sign url = %q", task.SignURL)
	}
	if _, err := s.CreateSignTask(t.Context(), SignTaskRequest{}); err == nil {
		t.Fatal("missing signer should be rejected")
	}
}

func TestCreateSignTaskIsIdempotentPerContract(t *testing.T) {
	s := newTestSandbox(t)
	req := SignTaskRequest{ContractID: "c-1", SignerName: "Alice"}
	first, err := s.CreateSignTask(t.Context(), req)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	second, err := s.CreateSignTask(t.Context(), req)
	if err != nil {
		t.Fatalf("repeat create task: %v", err)
	}
	if first.TaskID != second.TaskID || first.SignURL != second.SignURL {
		t.Fatalf("retry created a second task: %q vs %q", first.TaskID, second.TaskID)
	}
	other, err := s.CreateSignTask(t.Context(), SignTaskRequest{ContractID: "c-2", SignerName: "Bob"})
	if err != nil {
		t.Fatalf("create second contract task: %v", err)
	}
	if other.TaskID == first.TaskID {
		t.Fatal("different contracts must not share a task")
	}
}

func TestCancelSignTaskAllowsFreshTask(t *testing.T) {
	s := newTestSandbox(t)
	req := SignTaskRequest{ContractID: "c-1", SignerName: "Alice"}
	first, err := s.CreateSignTask(t.Context(), req)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := s.CancelSignTask(t.Context(), first.TaskID); err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if err := s.CancelSignTask(t.Context(), first.TaskID); err == nil {
		t.Fatal("cancelling an unknown task should fail")
	}
	second, err := s.CreateSignTask(t.Context(), req)
	if err != nil {
		t.Fatalf("create task after cancel: %v", err)
	}
	if second.TaskID == first.TaskID {
		t.Fatal("a cancelled task must not be handed out again")
	}
}

func TestVerifyCallbackValid(t *testing.T) {
	s := newTestSandbox(t)
	body, _ := json.Marshal(map[string]any{
		"event_id": "evt_1", "sign_task_id": "sbx_abc", "type": "signed",
		"signer_name": "Alice", "document_sha256": strings.Repeat("a", 64),
		"signed_at": "2026-10-01T09:00:00Z",
	})
	ev, err := s.VerifyCallback(signedRequest(t, s, body, s.now()), body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.EventID != "evt_1" || ev.Type != EventSigned || ev.SignerName != "Alice" {
		t.Fatalf("event = %+v", ev)
	}
	if len(ev.Raw) == 0 {
		t.Fatal("raw payload should be preserved")
	}
}

func TestVerifyCallbackRejectsTamperedBody(t *testing.T) {
	s := newTestSandbox(t)
	body, _ := json.Marshal(map[string]any{"event_id": "evt_1", "sign_task_id": "sbx_abc", "type": "signing"})
	r := signedRequest(t, s, body, s.now())
	tampered := append([]byte(nil), body...)
	tampered[2] = 'X'
	_, err := s.VerifyCallback(r, tampered)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v, want invalid signature", err)
	}
}

func TestVerifyCallbackRejectsMissingHeader(t *testing.T) {
	s := newTestSandbox(t)
	body := []byte(`{"event_id":"e","sign_task_id":"t","type":"signing"}`)
	r := httptest.NewRequest(http.MethodPost, "/webhooks/esign/sandbox", nil)
	_, err := s.VerifyCallback(r, body)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v, want invalid signature", err)
	}
}

func TestVerifyCallbackRejectsStaleAndFutureTimestamps(t *testing.T) {
	s := newTestSandbox(t)
	body := []byte(`{"event_id":"e","sign_task_id":"t","type":"signing"}`)
	for name, at := range map[string]time.Time{
		"stale":  s.now().Add(-10 * time.Minute),
		"future": s.now().Add(10 * time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.VerifyCallback(signedRequest(t, s, body, at), body)
			if !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("err = %v, want invalid signature", err)
			}
		})
	}
}

func TestVerifyCallbackRejectsMalformedPayloads(t *testing.T) {
	s := newTestSandbox(t)
	cases := map[string]string{
		"not json":         `{`,
		"unknown type":     `{"event_id":"e","sign_task_id":"t","type":"approved"}`,
		"missing event id": `{"sign_task_id":"t","type":"signing"}`,
		"signed no signer": `{"event_id":"e","sign_task_id":"t","type":"signed"}`,
		"bad document":     `{"event_id":"e","sign_task_id":"t","type":"signed","signer_name":"A","document_sha256":"xyz","signed_at":"2026-10-01T09:00:00Z"}`,
		"signed no hash":   `{"event_id":"e","sign_task_id":"t","type":"signed","signer_name":"A","signed_at":"2026-10-01T09:00:00Z"}`,
		"signed no time":   `{"event_id":"e","sign_task_id":"t","type":"signed","signer_name":"A","document_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []byte(body)
			_, err := s.VerifyCallback(signedRequest(t, s, raw, s.now()), raw)
			if !errors.Is(err, ErrMalformedCallback) {
				t.Fatalf("err = %v, want malformed callback", err)
			}
		})
	}
}
