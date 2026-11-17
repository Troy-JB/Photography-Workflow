package schedule

type BookingStatus string

const (
	StatusHeld      BookingStatus = "held"
	StatusConfirmed BookingStatus = "confirmed"
	StatusCancelled BookingStatus = "cancelled"
	StatusExpired   BookingStatus = "expired"
)

const (
	KindPhotographer = "photographer"
	KindStudio       = "studio"
	KindMakeup       = "makeup"
	KindAssistant    = "assistant"
	KindEquipment    = "equipment"
)

const (
	BlockHold    = "hold"
	BlockBooking = "booking"
	BlockManual  = "manual"
	BlockClosure = "closure"
)

func ValidResourceKind(kind string) bool {
	switch kind {
	case KindPhotographer, KindStudio, KindMakeup, KindAssistant, KindEquipment:
		return true
	}
	return false
}

// ValidManualBlockKind limits the block endpoint to operator-owned blocks.
// Hold and booking blocks are written by the booking aggregate.
func ValidManualBlockKind(kind string) bool {
	return kind == BlockManual || kind == BlockClosure
}

func ValidBookingStatus(status string) bool {
	switch BookingStatus(status) {
	case StatusHeld, StatusConfirmed, StatusCancelled, StatusExpired:
		return true
	}
	return false
}

// IsActive reports whether the booking still occupies its resources.
func IsActive(status BookingStatus) bool {
	return status == StatusHeld || status == StatusConfirmed
}

// CanConfirm reports whether a held booking may be confirmed. Callers pass
// expired=true when hold_expires_at has lapsed even if the worker has not
// swept the row yet.
func CanConfirm(status BookingStatus, expired bool) bool {
	return status == StatusHeld && !expired
}

func CanReschedule(status BookingStatus) bool {
	return status == StatusHeld || status == StatusConfirmed
}

func CanCancel(status BookingStatus) bool {
	return status == StatusHeld || status == StatusConfirmed
}
