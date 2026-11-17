package device

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureCreatesStableIdentity(t *testing.T) {
	dir := t.TempDir()
	id, err := Ensure(dir, "Edit Bay 1")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !strings.HasPrefix(id.Fingerprint(), "pwf-") {
		t.Fatalf("fingerprint = %q", id.Fingerprint())
	}
	if id.PublicKey() == "" || id.PrivateKey() == nil {
		t.Fatal("device key pair must be created")
	}
	if id.Name() != "Edit Bay 1" {
		t.Fatalf("name = %q", id.Name())
	}
	if _, err := os.Stat(id.KeyPath()); err != nil {
		t.Fatalf("key file missing: %v", err)
	}

	again, err := Ensure(dir, "Renamed")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again.Fingerprint() != id.Fingerprint() {
		t.Fatalf("fingerprint changed: %q → %q", id.Fingerprint(), again.Fingerprint())
	}
	if again.PublicKey() != id.PublicKey() {
		t.Fatal("key pair must survive a restart")
	}
	if again.Name() != "Edit Bay 1" {
		t.Fatalf("existing name must win, got %q", again.Name())
	}
}

func TestEnsureDefaultName(t *testing.T) {
	id, err := Ensure(t.TempDir(), "  ")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id.Name() != "PWF Agent" {
		t.Fatalf("name = %q", id.Name())
	}
}

func TestEnsurePersistsServerID(t *testing.T) {
	dir := t.TempDir()
	id, err := Ensure(dir, "Desk")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := id.SetServerID("dev-123"); err != nil {
		t.Fatalf("set server id: %v", err)
	}
	again, err := Ensure(dir, "Desk")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again.ServerID() != "dev-123" {
		t.Fatalf("server id = %q", again.ServerID())
	}
}

func TestEnsureRegeneratesCorruptKey(t *testing.T) {
	dir := t.TempDir()
	id, err := Ensure(dir, "Desk")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := id.SetServerID("dev-123"); err != nil {
		t.Fatalf("set server id: %v", err)
	}
	if err := os.WriteFile(id.KeyPath(), []byte("not a private key"), 0o600); err != nil {
		t.Fatalf("corrupt key: %v", err)
	}

	fresh, err := Ensure(dir, "Desk")
	if err != nil {
		t.Fatalf("ensure after corruption: %v", err)
	}
	if fresh.Fingerprint() != id.Fingerprint() {
		t.Fatal("the installation identity must survive a key rotation")
	}
	if fresh.PublicKey() == id.PublicKey() {
		t.Fatal("a corrupt key must be replaced")
	}
	if fresh.ServerID() != "" {
		t.Fatalf("server id must be cleared after rotation, got %q", fresh.ServerID())
	}
}

func TestEnsureRegeneratesCorruptMeta(t *testing.T) {
	dir := t.TempDir()
	id, err := Ensure(dir, "Desk")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, metaFile), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}
	fresh, err := Ensure(dir, "Desk")
	if err != nil {
		t.Fatalf("ensure after corrupt meta: %v", err)
	}
	if fresh.Fingerprint() == "" || fresh.PublicKey() == "" {
		t.Fatal("identity must be rebuilt")
	}
	if fresh.Fingerprint() == id.Fingerprint() {
		t.Fatal("corrupt meta carries no installation id; a new one is expected")
	}
}

func TestEnsureRejectsEmptyDir(t *testing.T) {
	if _, err := Ensure("", "Desk"); err == nil {
		t.Fatal("empty directory must be rejected")
	}
}
