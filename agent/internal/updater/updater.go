// Package updater verifies and downloads signed agent releases and decides
// when a version must be rolled back after repeated startup failures.
//
// Trust is anchored in a pinned Ed25519 public key: the manifest is signed
// over its canonical JSON encoding, and the installer's SHA-256 is checked
// after download. The stable channel additionally requires a code-signed
// installer; see authenticode_windows.go.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrNotConfigured means this build has no manifest URL or signing key, so
// update checks are disabled rather than failing with a network error.
var ErrNotConfigured = errors.New("此构建未配置更新源")

// ErrNoUpdate is returned by Check when the server offers nothing newer.
var ErrNoUpdate = errors.New("已是最新版本")

// ErrAuthenticodeUnsupported is returned on platforms without Authenticode.
var ErrAuthenticodeUnsupported = errors.New("当前平台不支持代码签名校验")

// maxManifestBytes bounds the signed document; manifests are a few KiB.
const maxManifestBytes = 1 << 20

// maxInstallerBytes bounds a downloaded installer to catch runaway responses.
const maxInstallerBytes = 2 << 30

// Manifest is the signed description of one release.
type Manifest struct {
	Version     string    `json:"version"`
	Channel     string    `json:"channel"`
	URL         string    `json:"url"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	MinVersion  string    `json:"min_version,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
}

// Envelope is the on-disk manifest file: the payload plus its signature.
type Envelope struct {
	Manifest  Manifest `json:"manifest"`
	Signature string   `json:"signature"`
}

// Canonical returns the exact bytes covered by the signature. Signing tools
// must use the same encoding.
func (m Manifest) Canonical() ([]byte, error) {
	return json.Marshal(m)
}

// Sign produces an envelope for m using priv.
func Sign(priv ed25519.PrivateKey, m Manifest) ([]byte, error) {
	payload, err := m.Canonical()
	if err != nil {
		return nil, err
	}
	env := Envelope{Manifest: m, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// ParseEnvelope verifies the signature and the channel, then validates the
// manifest fields. It never trusts the file before the signature checks out.
func ParseEnvelope(data []byte, pub ed25519.PublicKey, channel string) (Manifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return Manifest{}, ErrNotConfigured
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Manifest{}, fmt.Errorf("更新清单格式无效：%w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Manifest{}, errors.New("更新清单签名格式无效")
	}
	payload, err := env.Manifest.Canonical()
	if err != nil {
		return Manifest{}, fmt.Errorf("更新清单无法规范化：%w", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return Manifest{}, errors.New("更新清单签名校验失败")
	}
	if err := env.Manifest.validate(channel); err != nil {
		return Manifest{}, err
	}
	return env.Manifest, nil
}

func (m Manifest) validate(channel string) error {
	if err := CheckVersion(m.Version); err != nil {
		return fmt.Errorf("更新清单版本号无效：%w", err)
	}
	if m.Channel != channel {
		return fmt.Errorf("更新清单通道为 %q，期望 %q", m.Channel, channel)
	}
	if err := CheckDownloadURL(m.URL); err != nil {
		return fmt.Errorf("更新下载地址无效：%w", err)
	}
	sum, err := hex.DecodeString(m.SHA256)
	if err != nil || len(sum) != sha256.Size {
		return errors.New("更新清单缺少有效的 SHA-256")
	}
	if m.Size < 0 {
		return errors.New("更新清单的大小无效")
	}
	if m.MinVersion != "" {
		if err := CheckVersion(m.MinVersion); err != nil {
			return fmt.Errorf("更新清单 min_version 无效：%w", err)
		}
	}
	return nil
}

// CheckDownloadURL requires HTTPS; plain HTTP is accepted only for the
// loopback host used by tests and local rigs.
func CheckDownloadURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return errors.New("下载地址必须使用 https://")
		}
	default:
		return errors.New("下载地址必须使用 https://")
	}
	if u.Host == "" {
		return errors.New("下载地址缺少主机名")
	}
	if u.User != nil {
		return errors.New("下载地址不能包含用户名或密码")
	}
	return nil
}

// normalizeThumbprint accepts hex with or without colons and spaces.
func normalizeThumbprint(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, " ", "")
	return s
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Result is one completed update check.
type Result struct {
	Manifest  Manifest
	Available bool
	// Mandatory marks a release the server says everyone below MinVersion
	// must install.
	Mandatory bool
}

// Client performs signed update checks and downloads.
type Client struct {
	HTTP        *http.Client
	ManifestURL string
	PublicKey   ed25519.PublicKey
	Channel     string
	// OnProgress, when set, receives the number of bytes written while
	// Download runs plus the expected total (0 when unknown).
	OnProgress func(done, total int64)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// No whole-request timeout: a 200MB installer on a slow link is a
	// healthy download. The caller's context bounds the total time and the
	// transport fails fast only when the server never answers.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: tr}
}

// Check fetches and verifies the manifest, then compares it with current.
func (c *Client) Check(ctx context.Context, current string) (Result, error) {
	if err := CheckDownloadURL(c.ManifestURL); err != nil {
		return Result{}, ErrNotConfigured
	}
	if len(c.PublicKey) != ed25519.PublicKeySize || strings.TrimSpace(c.Channel) == "" {
		return Result{}, ErrNotConfigured
	}
	if err := CheckVersion(current); err != nil {
		return Result{}, fmt.Errorf("当前版本号无效：%w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ManifestURL, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("检查更新失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("检查更新失败：服务器返回 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("读取更新清单失败：%w", err)
	}
	if len(data) > maxManifestBytes {
		return Result{}, errors.New("更新清单过大，已拒绝")
	}
	m, err := ParseEnvelope(data, c.PublicKey, c.Channel)
	if err != nil {
		return Result{}, err
	}
	res := Result{Manifest: m, Available: Compare(m.Version, current) > 0}
	if res.Available && m.MinVersion != "" && Compare(current, m.MinVersion) < 0 {
		res.Mandatory = true
	}
	return res, nil
}

// Download stores the installer in dir and returns its path. Reusing an
// existing file is only allowed after its hash matches the manifest.
func (c *Client) Download(ctx context.Context, m Manifest, dir string) (string, error) {
	if err := CheckDownloadURL(m.URL); err != nil {
		return "", err
	}
	if err := CheckVersion(m.Version); err != nil {
		return "", err
	}
	want, err := hex.DecodeString(m.SHA256)
	if err != nil || len(want) != sha256.Size {
		return "", errors.New("更新清单缺少有效的 SHA-256")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建更新目录失败：%w", err)
	}
	final := filepath.Join(dir, "PWF-Agent-"+m.Version+".exe")
	if ok, _ := HashMatches(final, m.SHA256); ok {
		if c.OnProgress != nil && m.Size > 0 {
			c.OnProgress(m.Size, m.Size)
		}
		return final, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("下载更新失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载更新失败：服务器返回 %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".pwf-update-*.part")
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	total := m.Size
	if total <= 0 {
		total = resp.ContentLength
	}
	var body io.Reader = resp.Body
	if c.OnProgress != nil {
		body = &progressReader{r: resp.Body, total: total, cb: c.OnProgress}
	}
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, maxInstallerBytes+1))
	if copyErr == nil && c.OnProgress != nil {
		c.OnProgress(n, n)
	}
	if copyErr == nil && n > maxInstallerBytes {
		copyErr = errors.New("安装包超过大小上限")
	}
	if copyErr != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("下载更新失败：%w", copyErr)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("写入安装包失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("关闭安装包失败：%w", err)
	}
	if m.Size > 0 && n != m.Size {
		return "", fmt.Errorf("安装包大小不符：得到 %d 字节，期望 %d", n, m.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(m.SHA256) {
		return "", fmt.Errorf("安装包 SHA-256 校验失败：得到 %s", got)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("保存安装包失败：%w", err)
	}
	return final, nil
}

// progressReader reports byte counts while Download copies the body. Updates
// are coalesced because a 100 MiB installer produces thousands of reads.
type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	last  time.Time
	cb    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		if now := time.Now(); now.Sub(p.last) >= 200*time.Millisecond {
			p.last = now
			p.cb(p.done, p.total)
		}
	}
	return n, err
}

// HashMatches reports whether the file at path has the given hex SHA-256.
func HashMatches(path, wantHex string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxInstallerBytes+1)); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), wantHex), nil
}

// ---------- versions ----------

// CheckVersion validates a "1.2.3" style version with an optional prerelease
// suffix and build metadata.
func CheckVersion(v string) error {
	_, _, err := splitVersion(v)
	return err
}

// Compare orders versions the way a release channel needs: numeric segments
// first, then SemVer-style prerelease rules. Build metadata is ignored.
func Compare(a, b string) int {
	an, apre, err := splitVersion(a)
	if err != nil {
		return strings.Compare(a, b)
	}
	bn, bpre, err := splitVersion(b)
	if err != nil {
		return strings.Compare(a, b)
	}
	for i := 0; i < len(an) || i < len(bn); i++ {
		x, y := 0, 0
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	if apre == "" && bpre != "" {
		return 1
	}
	if apre != "" && bpre == "" {
		return -1
	}
	if apre == bpre {
		return 0
	}
	return comparePrerelease(apre, bpre)
}

func comparePrerelease(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		if i >= len(as) {
			return -1
		}
		if i >= len(bs) {
			return 1
		}
		ai, aerr := strconv.Atoi(as[i])
		bi, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		case aerr == nil:
			return -1 // numeric identifiers sort before alphanumeric
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return 0
}

func splitVersion(v string) ([]int, string, error) {
	orig := v
	v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	pre := ""
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
		if pre == "" {
			return nil, "", fmt.Errorf("版本号 %q 的预发布后缀为空", orig)
		}
	}
	if v == "" {
		return nil, "", fmt.Errorf("版本号 %q 为空", orig)
	}
	parts := strings.Split(v, ".")
	if len(parts) > 4 {
		return nil, "", fmt.Errorf("版本号 %q 段数过多", orig)
	}
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return nil, "", fmt.Errorf("版本号 %q 含空段", orig)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return nil, "", fmt.Errorf("版本号 %q 含非数字段", orig)
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, "", fmt.Errorf("版本号 %q 无效", orig)
		}
		nums = append(nums, n)
	}
	for _, r := range pre {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-') {
			return nil, "", fmt.Errorf("版本号 %q 的预发布后缀含非法字符", orig)
		}
	}
	return nums, strings.ToLower(pre), nil
}
