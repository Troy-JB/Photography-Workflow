package contract

const (
	StatusDraft    = "draft"
	StatusRendered = "rendered"
	// StatusSubmitting reserves the contract while the e-sign provider creates
	// the signing task. It is internal: sign URLs are not published and
	// callbacks are not accepted until the task is recorded.
	StatusSubmitting = "submitting"
	StatusSent       = "sent"
	StatusSigning    = "signing"
	StatusSigned     = "signed"
	StatusVoid       = "void"
)

func ValidStatus(s string) bool {
	switch s {
	case StatusDraft, StatusRendered, StatusSubmitting, StatusSent, StatusSigning, StatusSigned, StatusVoid:
		return true
	}
	return false
}

func CanSend(status string) bool {
	return status == StatusDraft || status == StatusRendered
}

// CanSign allows a signature callback to arrive directly (sent) or after a
// provider "signing" event.
func CanSign(status string) bool {
	return status == StatusSent || status == StatusSigning
}

// CanVoid only covers unsigned contracts. Voiding a signed contract needs a
// separate, audited business flow and must never be a side effect of a callback.
// A submitting contract is voidable so an operator can abort an in-flight send;
// the send then revokes the provider task it created.
func CanVoid(status string) bool {
	switch status {
	case StatusDraft, StatusRendered, StatusSubmitting, StatusSent, StatusSigning:
		return true
	}
	return false
}
