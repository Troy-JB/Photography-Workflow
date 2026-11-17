package delivery

import (
	"errors"
	"testing"
	"time"
)

func validGateState() *tokenState {
	return &tokenState{
		ExpiresAt:     time.Now().Add(time.Hour),
		MaxDownloads:  10,
		DeliveryState: StatusReady,
		BuildVersion:  1,
		PaymentStatus: "paid",
		ProjectState:  "completed",
		RoundStatus:   "approved",
	}
}

func TestCheckTokenGate(t *testing.T) {
	now := time.Now()
	revoked := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	past := now.Add(-time.Minute)

	tests := []struct {
		name   string
		mutate func(*tokenState)
		want   error
	}{
		{"ready", func(*tokenState) {}, nil},
		{"project in progress", func(st *tokenState) { st.ProjectState = "in_progress" }, nil},
		{"revoked wins over expiry", func(st *tokenState) {
			st.RevokedAt = &revoked
			st.PaymentStatus = "unpaid"
			st.ExpiresAt = past
		}, ErrTokenRevoked},
		{"expired", func(st *tokenState) { st.ExpiresAt = past }, ErrTokenExpired},
		{"expires exactly now", func(st *tokenState) { st.ExpiresAt = now }, ErrTokenExpired},
		{"archived project", func(st *tokenState) { st.ProjectState = "archived" }, ErrProjectArchived},
		{"cancelled project", func(st *tokenState) { st.ProjectState = "cancelled" }, ErrProjectCancelled},
		{"archived before payment check", func(st *tokenState) {
			st.ProjectState = "archived"
			st.PaymentStatus = "unpaid"
		}, ErrProjectArchived},
		{"final payment outstanding", func(st *tokenState) { st.PaymentStatus = "deposit_paid" }, ErrPaymentRequired},
		{"refunded", func(st *tokenState) { st.PaymentStatus = "refunded" }, ErrPaymentRequired},
		{"round not approved", func(st *tokenState) { st.RoundStatus = "changes_requested" }, ErrRoundNotApproved},
		{"payment checked before round", func(st *tokenState) {
			st.PaymentStatus = "unpaid"
			st.RoundStatus = "in_review"
		}, ErrPaymentRequired},
		{"blocked for payment", func(st *tokenState) {
			st.DeliveryState = StatusBlocked
			st.BlockedReason = BlockedReasonPayment
		}, ErrPaymentRequired},
		{"blocked for other reason", func(st *tokenState) {
			st.DeliveryState = StatusBlocked
			st.BlockedReason = "round_changed"
		}, ErrDeliveryNotReady},
		{"building", func(st *tokenState) { st.DeliveryState = StatusBuilding }, ErrDeliveryNotReady},
		{"failed", func(st *tokenState) { st.DeliveryState = StatusFailed }, ErrDeliveryNotReady},
		{"pending", func(st *tokenState) { st.DeliveryState = StatusPending }, ErrDeliveryNotReady},
		{"token expiry after delivery state", func(st *tokenState) {
			st.ExpiresAt = future
			st.DeliveryState = StatusReady
		}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := validGateState()
			tt.mutate(st)
			err := checkTokenGate(st, now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("checkTokenGate() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCheckProjectState(t *testing.T) {
	for state, want := range map[string]error{
		"in_progress": nil,
		"completed":   nil,
		"delivered":   nil,
		"archived":    ErrProjectArchived,
		"cancelled":   ErrProjectCancelled,
	} {
		if err := checkProjectState(state); !errors.Is(err, want) {
			t.Fatalf("checkProjectState(%q) = %v, want %v", state, err, want)
		}
	}
}
