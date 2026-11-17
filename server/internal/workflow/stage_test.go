package workflow

import (
	"testing"
	"time"
)

func TestDeriveStage(t *testing.T) {
	tests := []struct {
		name string
		in   Milestones
		want Stage
	}{
		{"unpaid", Milestones{PaymentStatus: "unpaid"}, StageNotShot},
		{"unpaid despite gallery", Milestones{PaymentStatus: "unpaid", GalleryStatus: "published"}, StageNotShot},
		{"unpaid despite submitted selection", Milestones{PaymentStatus: "unpaid", Submitted: true}, StageNotShot},
		{"deposit paid", Milestones{PaymentStatus: "deposit_paid"}, StageShooting},
		{"change balance pending", Milestones{PaymentStatus: "change_pending"}, StageShooting},
		{"originals published", Milestones{PaymentStatus: "deposit_paid", GalleryStatus: "published"}, StageOriginals},
		{"shoot date passed before gallery publication", Milestones{PaymentStatus: "deposit_paid", ShootDatePassed: true}, StageOriginals},
		{"shoot date passed with balance pending", Milestones{PaymentStatus: "balance_pending", ShootDatePassed: true}, StageOriginals},
		{"shoot date passed while unpaid", Milestones{PaymentStatus: "unpaid", ShootDatePassed: true}, StageNotShot},
		{"selection remains selecting after shoot date", Milestones{PaymentStatus: "deposit_paid", ShootDatePassed: true, SelectedCount: 1}, StageSelecting},
		{"selection started", Milestones{PaymentStatus: "deposit_paid", GalleryStatus: "published", SelectedCount: 1}, StageSelecting},
		{"selection submitted", Milestones{PaymentStatus: "deposit_paid", Submitted: true}, StageRetouchDelivery},
		{"proof in progress", Milestones{PaymentStatus: "deposit_paid", ProofStatus: "retouching"}, StageRetouchDelivery},
		{"delivery ready but unpaid", Milestones{PaymentStatus: "deposit_paid", DeliveryStatus: "ready"}, StageRetouchDelivery},
		{"paid and delivered", Milestones{PaymentStatus: "paid", DeliveryStatus: "ready"}, StageCompleted},
		{"cancelled wins", Milestones{ProjectState: "cancelled", PaymentStatus: "paid", DeliveryStatus: "ready"}, StageCancelled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveStage(tt.in); got != tt.want {
				t.Fatalf("DeriveStage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDateHasPassedUsesConfiguredTimezoneAndCalendarDate(t *testing.T) {
	tz, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	shoot := time.Date(2026, 11, 5, 18, 0, 0, 0, tz)

	if dateHasPassed(&shoot, time.Date(2026, 11, 5, 23, 59, 0, 0, tz), tz) {
		t.Fatal("shoot date should not have passed before local midnight")
	}
	if !dateHasPassed(&shoot, time.Date(2026, 11, 6, 0, 0, 0, 0, tz), tz) {
		t.Fatal("shoot date should have passed at the next local calendar day")
	}
}
