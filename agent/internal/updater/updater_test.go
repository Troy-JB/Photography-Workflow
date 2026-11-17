package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func testManifest(url string, payload []byte) Manifest {
	sum := sha256.Sum256(payload)
	return Manifest{
		Version: "0.5.0", Channel: "dev", URL: url,
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload)),
		Notes: "test release", MinVersion: "0.4.0",
	}
}

func TestParseEnvelopeRejectsTampering(t *testing.T) {
	pub, priv := testKey(t)
	m := testManifest("https://download.example.com/agent/windows/dev/PWF-Agent-0.5.0.exe", []byte("payload"))
	signed, err := Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnvelope(signed, pub, "dev")
	if err != nil {
		t.Fatalf("parse signed manifest: %v", err)
	}
	if got.Version != m.Version || got.SHA256 != m.SHA256 {
		t.Fatalf("parsed = %+v", got)
	}

	var env Envelope
	if err := json.Unmarshal(signed, &env); err != nil {
		t.Fatal(err)
	}
	env.Manifest.Notes = "tampered"
	bad, _ := json.Marshal(env)
	if _, err := ParseEnvelope(bad, pub, "dev"); err == nil || !strings.Contains(err.Error(), "签名") {
		t.Fatalf("tampered manifest must fail signature, got %v", err)
	}

	if _, err := ParseEnvelope(signed, pub, "stable"); err == nil {
		t.Fatal("channel mismatch must be rejected")
	}

	m.URL = "http://download.example.com/x.exe"
	plain, _ := Sign(priv, m)
	if _, err := ParseEnvelope(plain, pub, "dev"); err == nil {
		t.Fatal("remote http download must be rejected")
	}

	m.URL = "https://download.example.com/x.exe"
	m.SHA256 = "zz"
	badSum, err := Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEnvelope(badSum, pub, "dev"); err == nil {
		t.Fatal("invalid sha must be rejected")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.4.0", 0},
		{"0.4.1", "0.4.0", 1},
		{"0.4", "0.4.0", 0},
		{"v1.0.0", "1.0.0", 0},
		{"1.0.0", "0.9.9", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.2", "1.0.0-rc.1", 1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0+build.5", "1.0.0", 0},
		{"0.4.0-dev", "0.4.0", -1},
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q,%q)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, bad := range []string{"", "1..2", "1.x.3", "1.2.3-", "1.2.3.4.5"} {
		if CheckVersion(bad) == nil {
			t.Errorf("CheckVersion(%q) must fail", bad)
		}
	}
}

func TestCheckAgainstStub(t *testing.T) {
	pub, priv := testKey(t)
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manifest.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	m := testManifest("https://download.example.com/a.exe", []byte("x"))
	m.Channel = "dev"
	var err error
	body, err = Sign(priv, m)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{HTTP: srv.Client(), ManifestURL: srv.URL + "/manifest.json", PublicKey: pub, Channel: "dev"}

	res, err := c.Check(context.Background(), "0.3.0")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !res.Available || !res.Mandatory {
		t.Fatalf("below min_version must be mandatory: %+v", res)
	}

	res, err = c.Check(context.Background(), "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Available || res.Mandatory {
		t.Fatalf("at min_version must be optional: %+v", res)
	}

	res, err = c.Check(context.Background(), "0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	if res.Available {
		t.Fatal("same version must not be offered")
	}

	if _, err := c.Check(context.Background(), "0.5.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background(), "not-a-version"); err == nil {
		t.Fatal("invalid current version must be rejected")
	}

	// A server error must not be mistaken for "up to date".
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv404.Close()
	c404 := &Client{HTTP: srv404.Client(), ManifestURL: srv404.URL + "/manifest.json", PublicKey: pub, Channel: "dev"}
	if _, err := c404.Check(context.Background(), "0.4.0"); err == nil {
		t.Fatal("404 must be an error")
	}

	unconfigured := &Client{ManifestURL: "", Channel: "dev"}
	if _, err := unconfigured.Check(context.Background(), "0.4.0"); err != ErrNotConfigured {
		t.Fatalf("unconfigured err = %v", err)
	}
}

func TestDownloadVerifiesHash(t *testing.T) {
	payload := []byte("installer bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/PWF-Agent-0.5.0.exe" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := testManifest(srv.URL+"/PWF-Agent-0.5.0.exe", payload)
	c := &Client{HTTP: srv.Client()}
	dir := t.TempDir()
	path, err := c.Download(context.Background(), m, dir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !strings.HasSuffix(path, "PWF-Agent-0.5.0.exe") {
		t.Fatalf("path = %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("downloaded bytes = %q err=%v", got, err)
	}

	// A second download reuses the verified file without hitting the network.
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("network must not be used when the file is already verified")
	}))
	defer closed.Close()
	c2 := &Client{HTTP: closed.Client()}
	if _, err := c2.Download(context.Background(), m, dir); err != nil {
		t.Fatalf("reuse: %v", err)
	}

	// Corrupted content must fail and leave no final file.
	m2 := testManifest(srv.URL+"/PWF-Agent-0.5.0.exe", []byte("other"))
	m2.Version = "0.6.0"
	badDir := t.TempDir()
	if _, err := (&Client{HTTP: srv.Client()}).Download(context.Background(), m2, badDir); err == nil {
		t.Fatal("hash mismatch must fail")
	}
	if entries, _ := os.ReadDir(badDir); len(entries) != 0 {
		t.Fatalf("failed download left files: %v", entries)
	}

	m3 := testManifest(srv.URL+"/PWF-Agent-0.5.0.exe", payload)
	m3.Version = "0.7.0"
	m3.Size = int64(len(payload) + 1)
	if _, err := (&Client{HTTP: srv.Client()}).Download(context.Background(), m3, t.TempDir()); err == nil {
		t.Fatal("size mismatch must fail")
	}
}

func TestRollbackAfterRepeatedFailures(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"0.4.0", "0.5.0"} {
		dir := filepath.Join(root, v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "PWF Agent.exe"), []byte("exe"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(t.TempDir(), "start.json")

	prev, err := BeginStart(state, root, "0.5.0")
	if err != nil || prev != "" {
		t.Fatalf("first start prev=%q err=%v", prev, err)
	}
	prev, err = BeginStart(state, root, "0.5.0")
	if err != nil || prev != "0.4.0" {
		t.Fatalf("second start prev=%q err=%v", prev, err)
	}

	if err := MarkHealthy(state, "0.5.0"); err != nil {
		t.Fatal(err)
	}
	prev, err = BeginStart(state, root, "0.5.0")
	if err != nil || prev != "" {
		t.Fatalf("healthy build must not roll back: prev=%q err=%v", prev, err)
	}

	if err := WriteCurrent(root, "0.5.0"); err != nil {
		t.Fatal(err)
	}
	cur, err := ReadCurrent(root)
	if err != nil || cur != "0.5.0" {
		t.Fatalf("current=%q err=%v", cur, err)
	}
	prev, err = PreviousVersion(root, "0.5.0")
	if err != nil || prev != "0.4.0" {
		t.Fatalf("previous=%q err=%v", prev, err)
	}
	if _, err := PreviousVersion(root, "0.4.0"); err == nil {
		t.Fatal("no older version must error")
	}

	notice := filepath.Join(t.TempDir(), "rollback.json")
	if err := WriteRollbackNotice(notice, "0.5.0", "0.4.0"); err != nil {
		t.Fatal(err)
	}
	if n, ok := TakeRollbackNotice(notice); !ok || n.From != "0.5.0" || n.To != "0.4.0" {
		t.Fatalf("notice = %+v ok=%v", n, ok)
	}
	if _, ok := TakeRollbackNotice(notice); ok {
		t.Fatal("notice must be consumed once")
	}
}

func TestPruneVersionsKeepsRollbackTarget(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"0.3.0", "0.4.0", "0.5.0", "0.6.0"} {
		dir := filepath.Join(root, v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "PWF Agent.exe"), []byte("exe"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Incomplete installs and non-version directories are ignored entirely.
	if err := os.MkdirAll(filepath.Join(root, "0.7.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneVersions(root, "0.6.0", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want two old versions", removed)
	}
	for _, gone := range []string{"0.3.0", "0.4.0"} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed", gone)
		}
	}
	for _, kept := range []string{"0.5.0", "0.6.0", "0.7.0", "cache"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Fatalf("%s should be kept: %v", kept, err)
		}
	}
}

func TestInstallRootAndTarget(t *testing.T) {
	root := t.TempDir()
	verDir := filepath.Join(root, "1.2.3")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(verDir, "PWF Agent.exe")
	if err := os.WriteFile(exe, []byte("exe"), 0o644); err != nil {
		t.Fatal(err)
	}
	gotRoot, version, ok := InstallRoot(exe)
	if !ok || gotRoot != root || version != "1.2.3" {
		t.Fatalf("root=%q version=%q ok=%v", gotRoot, version, ok)
	}
	if _, _, ok := InstallRoot(filepath.Join(root, "bin", "pwf-agent.exe")); ok {
		t.Fatal("development layout must not be treated as an install")
	}
	if err := WriteCurrent(root, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	target, err := Target(root)
	if err != nil || target != exe {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if err := WriteCurrent(root, "1.2.4"); err != nil {
		t.Fatal(err)
	}
	if _, err := Target(root); err == nil {
		t.Fatal("missing version must fail")
	}
	if err := os.WriteFile(filepath.Join(root, "current.txt"), []byte("../../evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Target(root); err == nil {
		t.Fatal("path traversal in current.txt must fail")
	}
}

func TestResolveConfig(t *testing.T) {
	pub, _ := testKey(t)
	key := base64.StdEncoding.EncodeToString(pub)
	t.Setenv("PWF_UPDATE_MANIFEST", "https://download.example.com/manifest.json")
	t.Setenv("PWF_UPDATE_PUBLIC_KEY", key)
	t.Setenv("PWF_UPDATE_CHANNEL", "dev")
	t.Setenv("PWF_UPDATE_SIGNER_THUMBPRINT", "")

	cfg, err := ResolveConfig(t.TempDir())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Channel != "dev" || len(cfg.PublicKey) != ed25519.PublicKeySize {
		t.Fatalf("config = %+v", cfg)
	}

	t.Setenv("PWF_UPDATE_CHANNEL", "stable")
	if _, err := ResolveConfig(t.TempDir()); err == nil {
		t.Fatal("stable without a pinned signer must fail closed")
	}
	t.Setenv("PWF_UPDATE_SIGNER_THUMBPRINT", "aa:bb")
	if _, err := ResolveConfig(t.TempDir()); err != nil {
		t.Fatalf("stable with pin: %v", err)
	}

	t.Setenv("PWF_UPDATE_CHANNEL", "dev")
	t.Setenv("PWF_UPDATE_PUBLIC_KEY", "not-base64")
	if _, err := ResolveConfig(t.TempDir()); err == nil {
		t.Fatal("bad key must fail")
	}
	t.Setenv("PWF_UPDATE_PUBLIC_KEY", key)
	t.Setenv("PWF_UPDATE_MANIFEST", "http://example.com/manifest.json")
	if _, err := ResolveConfig(t.TempDir()); err == nil {
		t.Fatal("remote http manifest must fail")
	}
}

func TestHTTPClientHasNoGlobalTimeout(t *testing.T) {
	c := &Client{}
	hc := c.httpClient()
	if hc.Timeout != 0 {
		t.Fatalf("update downloads must not be capped at %v; a large installer on a slow link is healthy", hc.Timeout)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout == 0 {
		t.Fatalf("transport must still bound time-to-first-byte, got %#v", hc.Transport)
	}
}
