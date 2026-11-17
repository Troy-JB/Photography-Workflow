package settings

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadMissingReturnsBackupEnabledDefault(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.RequireBackup || s.Workers != 2 {
		t.Fatalf("unexpected default: %+v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	want := Settings{
		DestA: `D:\Photos`, DestB: `\\nas\backup`, RequireBackup: true,
		Workers: 4, ProjectCode: "WK-2026",
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX mode bits; NTFS ACLs inherit from %LOCALAPPDATA%.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("settings file is group/world readable: %v", info.Mode())
	}
}

func TestSaveOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := Save(path, Settings{DestA: `D:\A`, DestB: `D:\B`, RequireBackup: true, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Settings{DestA: `D:\A2`, DestB: `D:\B2`, RequireBackup: true, Workers: 3}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.DestA != `D:\A2` || got.Workers != 3 {
		t.Fatalf("second save not applied: %+v", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestLoadCorruptFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for corrupt settings")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		s    Settings
		ok   bool
	}{
		{"dual", Settings{DestA: `D:\A`, DestB: `D:\B`, RequireBackup: true, Workers: 2}, true},
		{"single", Settings{DestA: `D:\A`, RequireBackup: false, Workers: 2}, true},
		{"missing A", Settings{RequireBackup: true, DestB: `D:\B`, Workers: 2}, false},
		{"missing B while required", Settings{DestA: `D:\A`, RequireBackup: true, Workers: 2}, false},
		{"same path", Settings{DestA: `D:\A`, DestB: `d:\a\`, RequireBackup: true, Workers: 2}, false},
		{"https server", Settings{DestA: `D:\A`, RequireBackup: false, Workers: 2,
			ServerURL: "https://photo.example.com"}, true},
		{"loopback http server", Settings{DestA: `D:\A`, RequireBackup: false, Workers: 2,
			ServerURL: "http://127.0.0.1:8080"}, true},
		{"remote http server", Settings{DestA: `D:\A`, RequireBackup: false, Workers: 2,
			ServerURL: "http://photo.example.com"}, false},
		{"tenant without server", Settings{DestA: `D:\A`, RequireBackup: false, Workers: 2,
			TenantSlug: "studio"}, false},
		{"negative bandwidth", Settings{DestA: `D:\A`, RequireBackup: false, Workers: 2,
			BandwidthKbps: -1}, false},
		{"absurd bandwidth", Settings{DestA: `D:\A`, RequireBackup: false, Workers: 2,
			BandwidthKbps: maxBandwidthKbps + 1}, false},
	}
	for _, tc := range cases {
		err := tc.s.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestSaveConnectionBeforeDestinationsExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Save(path, Settings{DestA: `D:\A`, DestB: `D:\B`, RequireBackup: true, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	// A fresh profile has no destinations yet; signing in must still work.
	fresh := filepath.Join(t.TempDir(), "settings.json")
	s, err := SaveConnection(fresh, " https://photo.example.com/ ", " studio ", 2000)
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	if s.ServerURL != "https://photo.example.com" || s.TenantSlug != "studio" || s.BandwidthKbps != 2000 {
		t.Fatalf("connection = %+v", s)
	}
	if s.RequireBackup != true || s.Workers != 2 {
		t.Fatalf("unrelated defaults must survive: %+v", s)
	}
	if _, err := SaveConnection(fresh, "http://remote.example.com", "", 0); err == nil {
		t.Fatal("remote plain http must be rejected")
	}
}

func TestServerURLNormalizedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	raw := `{"require_backup":false,"workers":2,"server_url":" https://photo.example.com/api/ ","tenant_slug":" studio "}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ServerURL != "https://photo.example.com/api" {
		t.Fatalf("server url = %q", s.ServerURL)
	}
	if s.TenantSlug != "studio" {
		t.Fatalf("tenant slug = %q", s.TenantSlug)
	}
	if s.BandwidthKbps != 0 {
		t.Fatalf("bandwidth = %d, want unlimited by default", s.BandwidthKbps)
	}
}

func TestWorkersClampedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"workers":99,"require_backup":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workers != maxWorkers {
		t.Fatalf("workers = %d, want %d", s.Workers, maxWorkers)
	}
}
