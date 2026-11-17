package esign

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sandbox is the development provider. It does not talk to a real e-sign
// platform: it issues local sign URLs and authenticates callbacks with an
// HMAC-SHA256 scheme so the callback path (auth, replay protection, idempotency,
// projections) can be exercised end to end. Production uses a vendor adapter
// with the same Provider contract.
type Sandbox struct {
	secret    []byte
	signBase  string
	tolerance time.Duration
	now       func() time.Time

	mu    sync.Mutex
	tasks map[string]sandboxTask
}

type sandboxTask struct {
	TaskID  string
	SignURL string
}

func NewSandbox(secret, signBaseURL string, tolerance time.Duration) (*Sandbox, error) {
	if len(secret) < 16 {
		return nil, fmt.Errorf("esign callback secret must be at least 16 characters")
	}
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}
	signBaseURL = strings.TrimRight(strings.TrimSpace(signBaseURL), "/")
	if signBaseURL == "" {
		return nil, fmt.Errorf("esign.sign_base_url is required for the sandbox provider")
	}
	return &Sandbox{
		secret: []byte(secret), signBase: signBaseURL, tolerance: tolerance, now: time.Now,
		tasks: make(map[string]sandboxTask),
	}, nil
}

func (s *Sandbox) Name() string { return "sandbox" }

// CreateSignTask mirrors the vendor contract: a contract with a live task gets
// that task back, so a retried send never produces a second sign URL.
func (s *Sandbox) CreateSignTask(_ context.Context, req SignTaskRequest) (*SignTask, error) {
	if strings.TrimSpace(req.SignerName) == "" {
		return nil, fmt.Errorf("signer name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.ContractID != "" {
		if t, ok := s.tasks[req.ContractID]; ok {
			return &SignTask{TaskID: t.TaskID, SignURL: t.SignURL}, nil
		}
	}
	id, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	token, err := randomHex(18)
	if err != nil {
		return nil, err
	}
	task := sandboxTask{
		TaskID:  "sbx_" + id,
		SignURL: s.signBase + "/sandbox/sbx_" + id + "?token=" + token,
	}
	if req.ContractID != "" {
		s.tasks[req.ContractID] = task
	}
	return &SignTask{TaskID: task.TaskID, SignURL: task.SignURL}, nil
}

// CancelSignTask revokes a task that was never signed, matching the compensation
// path the contract service runs when it cannot record the task locally.
func (s *Sandbox) CancelSignTask(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for contractID, t := range s.tasks {
		if t.TaskID == taskID {
			delete(s.tasks, contractID)
			return nil
		}
	}
	return fmt.Errorf("signing task %s not found", taskID)
}

func (s *Sandbox) VerifyCallback(r *http.Request, body []byte) (*CallbackEvent, error) {
	ts, sig, err := parseSignatureHeader(r.Header.Get("X-PWF-Signature"))
	if err != nil {
		return nil, err
	}
	now := s.now()
	if now.Sub(ts) > s.tolerance || ts.Sub(now) > s.tolerance {
		return nil, fmt.Errorf("%w: timestamp outside tolerance", ErrInvalidSignature)
	}
	expected := sign(s.secret, ts, body)
	if !hmac.Equal(expected, sig) {
		return nil, fmt.Errorf("%w: signature mismatch", ErrInvalidSignature)
	}

	var ev CallbackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedCallback, err)
	}
	ev.Raw = append(json.RawMessage(nil), body...)
	if err := ValidateEvent(&ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// SignRequestBody is used by the sandbox signer tooling and smoke tests to
// produce a callback that VerifyCallback accepts.
func SignRequestBody(secret string, at time.Time, body []byte) string {
	ts := at.Unix()
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(sign([]byte(secret), at, body))
}

func sign(secret []byte, at time.Time, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(at.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}

func parseSignatureHeader(header string) (time.Time, []byte, error) {
	var (
		hasTS bool
		ts    int64
		sig   []byte
	)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			v, err := strconv.ParseInt(strings.TrimPrefix(part, "t="), 10, 64)
			if err != nil {
				return time.Time{}, nil, fmt.Errorf("%w: bad timestamp", ErrInvalidSignature)
			}
			ts, hasTS = v, true
		case strings.HasPrefix(part, "v1="):
			raw, err := hex.DecodeString(strings.TrimPrefix(part, "v1="))
			if err != nil || len(raw) != sha256.Size {
				return time.Time{}, nil, fmt.Errorf("%w: bad signature value", ErrInvalidSignature)
			}
			sig = raw
		}
	}
	if !hasTS || sig == nil {
		return time.Time{}, nil, fmt.Errorf("%w: missing t or v1", ErrInvalidSignature)
	}
	return time.Unix(ts, 0), sig, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return hex.EncodeToString(b), nil
}
