package device

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func p256Key(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func validRegister(t *testing.T) RegisterInput {
	t.Helper()
	return RegisterInput{
		Name:         "Studio workstation",
		Fingerprint:  "machine-7f3a9c",
		Platform:     "windows",
		AgentVersion: "0.4.0",
		PublicKey:    p256Key(t),
	}
}

func TestNormalizeRegister(t *testing.T) {
	valid := validRegister(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	rsaDER, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal rsa key: %v", err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate p384 key: %v", err)
	}
	p384DER, err := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	if err != nil {
		t.Fatalf("marshal p384 key: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*RegisterInput)
		wantErr bool
	}{
		{name: "valid", mutate: func(*RegisterInput) {}},
		{name: "trims and lowercases platform", mutate: func(in *RegisterInput) {
			in.Platform = "  Windows "
		}},
		{name: "missing name", mutate: func(in *RegisterInput) { in.Name = " " }, wantErr: true},
		{name: "long name", mutate: func(in *RegisterInput) { in.Name = strings.Repeat("名", 201) }, wantErr: true},
		{name: "missing fingerprint", mutate: func(in *RegisterInput) { in.Fingerprint = "" }, wantErr: true},
		{name: "long fingerprint", mutate: func(in *RegisterInput) { in.Fingerprint = strings.Repeat("a", 257) }, wantErr: true},
		{name: "control chars in fingerprint", mutate: func(in *RegisterInput) { in.Fingerprint = "a\tb" }, wantErr: true},
		{name: "missing version", mutate: func(in *RegisterInput) { in.AgentVersion = "" }, wantErr: true},
		{name: "long version", mutate: func(in *RegisterInput) { in.AgentVersion = strings.Repeat("1", 65) }, wantErr: true},
		{name: "missing key", mutate: func(in *RegisterInput) { in.PublicKey = "" }, wantErr: true},
		{name: "key not base64", mutate: func(in *RegisterInput) { in.PublicKey = "not base64!" }, wantErr: true},
		{name: "key is rsa", mutate: func(in *RegisterInput) {
			in.PublicKey = base64.StdEncoding.EncodeToString(rsaDER)
		}, wantErr: true},
		{name: "key is p384", mutate: func(in *RegisterInput) {
			in.PublicKey = base64.StdEncoding.EncodeToString(p384DER)
		}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.mutate(&in)
			got, err := normalizeRegister(in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("err = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Platform != "windows" {
				t.Fatalf("platform = %q, want lowercased windows", got.Platform)
			}
		})
	}
}

func TestFeaturesAreExplicit(t *testing.T) {
	f := Features()
	for _, key := range []string{"ingest_v1", "server_derivatives", "device_tokens"} {
		if _, ok := f[key]; !ok {
			t.Fatalf("features is missing %q", key)
		}
	}
	if !f["ingest_v1"] {
		t.Fatal("ingest_v1 must be advertised")
	}
}
