//go:build windows && !cgo

package updater

import (
	"os"
	"strings"
	"testing"
)

// TestAuthenticodeSignedBinary exercises WinVerifyTrust and the signer
// thumbprint extraction. It needs a known signed binary because CI runners
// vary; set PWF_TEST_SIGNED_EXE to e.g. C:\Windows\System32\notepad.exe.
func TestAuthenticodeSignedBinary(t *testing.T) {
	exe := os.Getenv("PWF_TEST_SIGNED_EXE")
	if exe == "" {
		t.Skip("PWF_TEST_SIGNED_EXE not set")
	}
	thumb, err := SignerThumbprint(exe)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	if len(thumb) != 40 {
		t.Fatalf("thumbprint = %q", thumb)
	}
	if err := VerifyAuthenticode(exe, thumb); err != nil {
		t.Fatalf("verify with pinned thumbprint: %v", err)
	}
	if err := VerifyAuthenticode(exe, strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "指纹") {
		t.Fatalf("wrong pin must fail with thumbprint mismatch, got %v", err)
	}
	if err := VerifyAuthenticode(exe, ""); err == nil {
		t.Fatal("empty pin must fail closed on stable")
	}
}

func TestAuthenticodeUnsignedBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAuthenticode(exe, strings.Repeat("AB", 20)); err == nil {
		t.Fatal("unsigned test binary must not verify")
	}
}
