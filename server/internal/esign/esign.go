package esign

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidSignature  = errors.New("invalid callback signature")
	ErrMalformedCallback = errors.New("malformed callback")
)

const (
	EventSigning = "signing"
	EventSigned  = "signed"
	EventVoid    = "void"
)

type SignTaskRequest struct {
	ContractID    string
	ProjectID     string
	Title         string
	SignerName    string
	SignerContact string
	ReturnURL     string
}

type SignTask struct {
	TaskID  string
	SignURL string
}

// CallbackEvent is the normalised payload every provider adapter produces.
// Raw keeps the original provider payload for the evidence trail.
type CallbackEvent struct {
	EventID        string          `json:"event_id"`
	TaskID         string          `json:"sign_task_id"`
	Type           string          `json:"type"`
	SignerName     string          `json:"signer_name"`
	SignedAt       *time.Time      `json:"signed_at,omitempty"`
	DocumentSHA256 string          `json:"document_sha256,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

type Provider interface {
	Name() string
	// CreateSignTask must be idempotent per ContractID: two calls for the same
	// contract return the same task instead of creating a second one. The
	// contract service retries after a crash between the vendor call and the
	// local record, and a non-idempotent adapter would leave orphaned tasks.
	CreateSignTask(ctx context.Context, req SignTaskRequest) (*SignTask, error)
	// VerifyCallback authenticates the request before the payload is trusted.
	// Implementations must reject stale timestamps and compare signatures in
	// constant time.
	VerifyCallback(r *http.Request, body []byte) (*CallbackEvent, error)
}

// TaskCanceller is implemented by providers that can revoke a signing task that
// has not been signed yet. The contract service uses it to compensate a local
// write failure after the vendor accepted the task.
type TaskCanceller interface {
	CancelSignTask(ctx context.Context, taskID string) error
}

// TaskEventResult is the normalised outcome for whichever aggregate owns the
// signing task (contract or change order).
type TaskEventResult struct {
	Status      string    `json:"status"`
	SubjectType string    `json:"subject_type"`
	SubjectID   uuid.UUID `json:"subject_id"`
}

func ValidateEvent(ev *CallbackEvent) error {
	if ev.EventID == "" || len(ev.EventID) > 128 {
		return fmt.Errorf("%w: event_id is required and must be at most 128 chars", ErrMalformedCallback)
	}
	if ev.TaskID == "" || len(ev.TaskID) > 256 {
		return fmt.Errorf("%w: sign_task_id is required", ErrMalformedCallback)
	}
	switch ev.Type {
	case EventSigning, EventSigned, EventVoid:
	default:
		return fmt.Errorf("%w: unknown event type %q", ErrMalformedCallback, ev.Type)
	}
	if ev.Type == EventSigned {
		if ev.SignerName == "" {
			return fmt.Errorf("%w: signer_name is required for a signed event", ErrMalformedCallback)
		}
		// The signed file hash is the integrity anchor stored with the contract;
		// without it a signed callback proves nothing about the document.
		if len(ev.DocumentSHA256) != 64 {
			return fmt.Errorf("%w: document_sha256 must be 64 hex chars for a signed event", ErrMalformedCallback)
		}
		if _, err := hex.DecodeString(ev.DocumentSHA256); err != nil {
			return fmt.Errorf("%w: document_sha256 is not hex", ErrMalformedCallback)
		}
		if ev.SignedAt == nil {
			return fmt.Errorf("%w: signed_at is required for a signed event", ErrMalformedCallback)
		}
	}
	return nil
}
