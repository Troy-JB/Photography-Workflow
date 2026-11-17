package updater

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Release identity is stamped at build time:
//
//	-ldflags "-X .../internal/updater.BuildManifestURL=https://... \
//	          -X .../internal/updater.BuildPublicKey=<base64> \
//	          -X .../internal/updater.BuildChannel=stable \
//	          -X .../internal/updater.BuildSignerThumbprint=<sha1>"
//
// The environment overrides below exist for test rigs; they cannot weaken
// trust because the manifest must still verify against the pinned key.
var (
	BuildManifestURL      = ""
	BuildPublicKey        = ""
	BuildChannel          = "dev"
	BuildSignerThumbprint = ""
)

// Config is the resolved update identity for this process.
type Config struct {
	ManifestURL      string
	PublicKey        ed25519.PublicKey
	Channel          string
	SignerThumbprint string
	// Dir receives downloaded installers.
	Dir string
}

// ResolveConfig merges build-time identity with environment overrides and
// fails closed: a stable build without a pinned signer refuses to check for
// or install updates.
func ResolveConfig(downloadDir string) (Config, error) {
	return NewConfig(
		envOr("PWF_UPDATE_MANIFEST", BuildManifestURL),
		envOr("PWF_UPDATE_PUBLIC_KEY", BuildPublicKey),
		envOr("PWF_UPDATE_CHANNEL", BuildChannel),
		envOr("PWF_UPDATE_SIGNER_THUMBPRINT", BuildSignerThumbprint),
		downloadDir,
	)
}

// NewConfig validates an explicit identity. The CLI uses it for local test
// rigs; production code goes through ResolveConfig.
func NewConfig(manifestURL, publicKeyB64, channel, signerThumbprint, downloadDir string) (Config, error) {
	c := Config{
		ManifestURL:      strings.TrimSpace(manifestURL),
		Channel:          strings.ToLower(strings.TrimSpace(channel)),
		SignerThumbprint: normalizeThumbprint(signerThumbprint),
		Dir:              downloadDir,
	}
	keyRaw := strings.TrimSpace(publicKeyB64)
	if keyRaw == "" || c.ManifestURL == "" {
		return Config{}, ErrNotConfigured
	}
	key, err := base64.StdEncoding.DecodeString(keyRaw)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Config{}, errors.New("更新公钥配置无效")
	}
	c.PublicKey = ed25519.PublicKey(key)
	if c.Channel == "" {
		c.Channel = "dev"
	}
	if c.Channel != "dev" && c.Channel != "stable" {
		return Config{}, fmt.Errorf("未知更新通道 %q", c.Channel)
	}
	if c.Channel == "stable" && c.SignerThumbprint == "" {
		return Config{}, errors.New("stable 通道未固定签名者指纹，更新已禁用")
	}
	if err := CheckDownloadURL(c.ManifestURL); err != nil {
		return Config{}, fmt.Errorf("更新清单地址无效：%w", err)
	}
	return c, nil
}

// Client builds a checker for the resolved config.
func (c Config) Client() *Client {
	return &Client{ManifestURL: c.ManifestURL, PublicKey: c.PublicKey, Channel: c.Channel}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
