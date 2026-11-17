package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"photography-workflow/agent/internal/updater"
)

func writeInstaller(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "PWF-Agent-Setup-0.5.0.exe")
	if err := os.WriteFile(path, []byte(strings.Repeat("installer-bytes", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKeygenSignVerify(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "release-key.json")
	if err := cmdKeygen([]string{"-out", keyPath}); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pub, priv, err := loadKey(keyPath)
	if err != nil {
		t.Fatalf("loadKey: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize || len(priv) != ed25519.PrivateKeySize {
		t.Fatal("unexpected key sizes")
	}
	if err := cmdKeygen([]string{"-out", keyPath}); err == nil {
		t.Fatal("keygen must refuse to overwrite an existing key")
	}

	installer := writeInstaller(t, dir)
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := cmdSign([]string{
		"-key", keyPath, "-installer", installer,
		"-version", "0.5.0", "-channel", "dev",
		"-url", "https://download.example.com/agent/windows/dev/PWF-Agent-Setup-0.5.0.exe",
		"-min-version", "0.4.0", "-notes", "fixes capture stalls",
		"-published-at", "2026-10-31T00:00:00Z", "-out", manifestPath,
	}); err != nil {
		t.Fatalf("sign: %v", err)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := updater.ParseEnvelope(data, pub, "dev")
	if err != nil {
		t.Fatalf("parse signed manifest: %v", err)
	}
	if m.Version != "0.5.0" || m.Channel != "dev" || m.MinVersion != "0.4.0" {
		t.Fatalf("manifest = %+v", m)
	}
	if m.Size != int64(len(strings.Repeat("installer-bytes", 100))) {
		t.Fatalf("size = %d", m.Size)
	}
	if m.Notes != "fixes capture stalls" {
		t.Fatalf("notes = %q", m.Notes)
	}
	if !m.PublishedAt.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("published_at = %s", m.PublishedAt)
	}

	if err := cmdVerify([]string{"-key", keyPath, "-channel", "dev", "-in", manifestPath}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := cmdVerify([]string{"-public-key", "notbase64", "-channel", "dev", "-in", manifestPath}); err == nil {
		t.Fatal("invalid public key must fail")
	}
	if err := cmdVerify([]string{"-key", keyPath, "-channel", "stable", "-in", manifestPath}); err == nil {
		t.Fatal("channel mismatch must fail verification")
	}

	// A manifest signed by another key must not verify against this public key.
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := updater.Sign(otherPriv, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := updater.ParseEnvelope(forged, pub, "dev"); err == nil {
		t.Fatal("forged manifest must fail")
	}
}

func TestSignRejectsBadInputs(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "release-key.json")
	if err := cmdKeygen([]string{"-out", keyPath}); err != nil {
		t.Fatal(err)
	}
	installer := writeInstaller(t, dir)
	out := filepath.Join(dir, "manifest.json")
	base := []string{
		"-key", keyPath, "-installer", installer,
		"-version", "0.5.0", "-channel", "dev",
		"-url", "https://download.example.com/PWF-Agent-Setup-0.5.0.exe", "-out", out,
	}
	cases := map[string][]string{
		"remote http":          append(append([]string{}, base...), "-url", "http://download.example.com/x.exe"),
		"bad version":          append(append([]string{}, base...), "-version", "0.5.x"),
		"min above":            append(append([]string{}, base...), "-min-version", "0.6.0"),
		"bad channel":          append(append([]string{}, base...), "-channel", "beta"),
		"stable no thumbprint": append(append([]string{}, base...), "-channel", "stable"),
		"missing installer":    append(append([]string{}, base...), "-installer", filepath.Join(dir, "nope.exe")),
	}
	for name, args := range cases {
		if err := cmdSign(args); err == nil {
			t.Errorf("%s: sign must fail", name)
		}
	}

	// A stable manifest requires a real Authenticode signature; on every
	// platform an unsigned fixture must be refused (Windows: trust failure,
	// elsewhere: unsupported).
	err := cmdSign(append(append([]string{}, base...),
		"-channel", "stable", "-signer-thumbprint", "AABBCCDD"))
	if err == nil {
		t.Fatal("stable sign of an unsigned installer must fail closed")
	}
}
