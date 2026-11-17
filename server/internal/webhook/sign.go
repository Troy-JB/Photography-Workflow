// Package webhook implements the shared `t=<unix>,v1=<hex>` webhook signature
// used by payment alerts and shoot reminders, so one receiver verifier works
// for every outbound notification.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrPermanentDelivery marks a receiver answer that retrying cannot fix (4xx).
var ErrPermanentDelivery = errors.New("webhook receiver rejected the notification")

// Signature renders the `t=<unix>,v1=<hex>` header value. The MAC covers the
// timestamp, a separator and the exact body bytes.
func Signature(secret string, at time.Time, body []byte) string {
	ts := at.Unix()
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(signatureMAC([]byte(secret), ts, body))
}

func signatureMAC(secret []byte, ts int64, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}

// VerifySignature validates a signature header against the raw body and the
// timestamp tolerance. Receivers (including the smoke listener) use it.
func VerifySignature(secret, header string, body []byte, at time.Time, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}
	var (
		hasTS bool
		ts    int64
		sig   []byte
	)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "t="):
			v, err := strconv.ParseInt(strings.TrimPrefix(part, "t="), 10, 64)
			if err != nil {
				return fmt.Errorf("bad signature timestamp")
			}
			ts, hasTS = v, true
		case strings.HasPrefix(part, "v1="):
			raw, err := hex.DecodeString(strings.TrimPrefix(part, "v1="))
			if err != nil || len(raw) != sha256.Size {
				return fmt.Errorf("bad signature value")
			}
			sig = raw
		}
	}
	if !hasTS || sig == nil {
		return fmt.Errorf("signature header is missing t or v1")
	}
	signedAt := time.Unix(ts, 0)
	if at.Sub(signedAt) > tolerance || signedAt.Sub(at) > tolerance {
		return fmt.Errorf("signature timestamp outside tolerance")
	}
	if !hmac.Equal(signatureMAC([]byte(secret), ts, body), sig) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}
