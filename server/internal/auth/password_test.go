package auth

import "testing"

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "" {
		t.Fatal("empty hash")
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("wrong password must not verify")
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, err := HashPassword("same-password")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same-password")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password should differ")
	}
}

func TestHashRejectsShortPassword(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected error for password shorter than 8 chars")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for _, encoded := range []string{"", "plain", "$argon2id$v=19$m=1,t=1,p=1$bad"} {
		if VerifyPassword(encoded, "whatever") {
			t.Fatalf("malformed hash %q must not verify", encoded)
		}
	}
}
