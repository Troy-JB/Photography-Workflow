package contract

import "testing"

func TestCanSend(t *testing.T) {
	for _, s := range []string{StatusDraft, StatusRendered} {
		if !CanSend(s) {
			t.Fatalf("%s should be sendable", s)
		}
	}
	for _, s := range []string{StatusSent, StatusSigning, StatusSigned, StatusVoid, StatusSubmitting} {
		if CanSend(s) {
			t.Fatalf("%s must not be sendable", s)
		}
	}
}

func TestCanSign(t *testing.T) {
	for _, s := range []string{StatusSent, StatusSigning} {
		if !CanSign(s) {
			t.Fatalf("%s should accept a signature", s)
		}
	}
	for _, s := range []string{StatusDraft, StatusRendered, StatusSubmitting, StatusSigned, StatusVoid} {
		if CanSign(s) {
			t.Fatalf("%s must not accept a signature", s)
		}
	}
}

func TestCanVoid(t *testing.T) {
	for _, s := range []string{StatusDraft, StatusRendered, StatusSent, StatusSigning, StatusSubmitting} {
		if !CanVoid(s) {
			t.Fatalf("%s should be voidable", s)
		}
	}
	for _, s := range []string{StatusSigned, StatusVoid} {
		if CanVoid(s) {
			t.Fatalf("%s must not be voidable", s)
		}
	}
}

func TestValidStatus(t *testing.T) {
	for _, s := range []string{StatusDraft, StatusRendered, StatusSubmitting, StatusSent, StatusSigning, StatusSigned, StatusVoid} {
		if !ValidStatus(s) {
			t.Fatalf("%s should be valid", s)
		}
	}
	if ValidStatus("expired") {
		t.Fatal("expired is not a contract state")
	}
}
