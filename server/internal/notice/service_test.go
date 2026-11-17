package notice

import "testing"

func TestValidType(t *testing.T) {
	valid := []string{
		TypeShootReminder,
		TypeHoldExpiry,
		TypeAuthIncomplete,
		TypeGalleryDeadline,
		TypeGallerySubmitted,
		TypeProofReady,
		TypeProofChanges,
		TypeProofResubmitted,
		TypeProofApproved,
		TypeProofDeadline,
		TypeRetouchDelivered,
	}
	for _, v := range valid {
		if !ValidType(v) {
			t.Errorf("ValidType(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "gallery_published", "SHOOT_REMINDER", "unknown"} {
		if ValidType(v) {
			t.Errorf("ValidType(%q) = true, want false", v)
		}
	}
}
