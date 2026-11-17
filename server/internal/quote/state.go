package quote

const (
	StatusDraft      = "draft"
	StatusSent       = "sent"
	StatusAccepted   = "accepted"
	StatusRejected   = "rejected"
	StatusExpired    = "expired"
	StatusSuperseded = "superseded"
)

func ValidStatus(s string) bool {
	switch s {
	case StatusDraft, StatusSent, StatusAccepted, StatusRejected, StatusExpired, StatusSuperseded:
		return true
	}
	return false
}

// Editable reports whether line items may still change. Sending a quote freezes
// it; a revision must be created instead.
func Editable(status string) bool { return status == StatusDraft }

func CanSend(status string) bool { return status == StatusDraft }

func CanAccept(status string) bool { return status == StatusSent }

func CanReject(status string) bool { return status == StatusSent }
