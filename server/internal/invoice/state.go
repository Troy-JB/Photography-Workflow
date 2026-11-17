package invoice

const (
	TypeNormal  = "normal"
	TypeSpecial = "special"

	StatusRequested = "requested"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusIssued    = "issued"
	StatusSent      = "sent"
	StatusVoided    = "voided"

	RedFlushPending = "pending"
	RedFlushIssued  = "issued"
)

func ValidType(v string) bool {
	return v == TypeNormal || v == TypeSpecial
}

func ValidStatus(v string) bool {
	switch v {
	case StatusRequested, StatusApproved, StatusRejected, StatusIssued, StatusSent, StatusVoided:
		return true
	}
	return false
}

func CanApprove(status string) bool { return status == StatusRequested }

func CanReject(status string) bool { return status == StatusRequested }

func CanIssue(status string) bool { return status == StatusApproved }

func CanSend(status string) bool { return status == StatusIssued }

func CanVoid(status string) bool {
	return status == StatusRequested || status == StatusApproved
}
