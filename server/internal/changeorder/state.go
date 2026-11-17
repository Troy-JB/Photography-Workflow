package changeorder

const (
	StatusDraft             = "draft"
	StatusAwaitingSignature = "awaiting_signature"
	StatusSigned            = "signed"
	StatusAwaitingPayment   = "awaiting_payment"
	StatusApplied           = "applied"
	StatusCancelled         = "cancelled"
	StatusReversed          = "reversed"
)

const (
	ItemExtraRetouch = "extra_retouch"
	ItemReshoot      = "reshoot"
	ItemTravel       = "travel"
	ItemRush         = "rush"
	ItemOther        = "other"
)

func ValidStatus(v string) bool {
	switch v {
	case StatusDraft, StatusAwaitingSignature, StatusSigned, StatusAwaitingPayment,
		StatusApplied, StatusCancelled, StatusReversed:
		return true
	}
	return false
}

func ValidItemKind(v string) bool {
	switch v {
	case ItemExtraRetouch, ItemReshoot, ItemTravel, ItemRush, ItemOther:
		return true
	}
	return false
}

// CanSend reports whether the amendment can be handed to the e-sign provider.
func CanSend(status string) bool { return status == StatusDraft }

// CanCancel covers every state before money is collected; applied and reversed
// amendments are financial history and are corrected through refunds instead.
func CanCancel(status string) bool {
	switch status {
	case StatusDraft, StatusAwaitingSignature, StatusSigned, StatusAwaitingPayment:
		return true
	}
	return false
}

// CanCollect reports whether a supplement payment order may be created.
func CanCollect(status string) bool {
	return status == StatusSigned || status == StatusAwaitingPayment
}

// CanApply reports whether a settled supplement payment may move the amendment
// to applied. StatusSigned is accepted because the payment can settle in the
// small window before the status CAS records the payment order.
func CanApply(status string) bool { return CanCollect(status) }

// CanReverse reports whether a fully refunded supplement payment reverses the
// applied amendment.
func CanReverse(status string) bool { return status == StatusApplied }
