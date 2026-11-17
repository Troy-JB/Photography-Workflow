package webhook

import (
	"strings"
	"testing"
	"time"
)

func TestSignatureRoundTrip(t *testing.T) {
	body := []byte(`{"event":"payment_alert","alert_id":"x"}`)
	at := time.Unix(1700000000, 0)
	header := Signature("0123456789abcdef", at, body)
	if !strings.HasPrefix(header, "t=1700000000,v1=") {
		t.Fatalf("signature header = %q", header)
	}
	if err := VerifySignature("0123456789abcdef", header, body, at, time.Minute); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := VerifySignature("other-secret-0123456789", header, body, at, time.Minute); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if err := VerifySignature("0123456789abcdef", header, []byte(`{}`), at, time.Minute); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := VerifySignature("0123456789abcdef", header, body, at.Add(2*time.Minute), time.Minute); err == nil {
		t.Fatal("stale timestamp accepted")
	}
}

func TestVerifySignatureRejectsMalformedHeaders(t *testing.T) {
	body := []byte(`{}`)
	at := time.Unix(1700000000, 0)
	for _, header := range []string{
		"",
		"t=1700000000",
		"v1=abcd",
		"t=not-a-number,v1=abcd",
		"t=1700000000,v1=zz",
		"t=1700000000,v1=" + strings.Repeat("0", 64),
	} {
		if err := VerifySignature("secret", header, body, at, time.Minute); err == nil {
			t.Fatalf("malformed header %q accepted", header)
		}
	}
}
