// Package device owns the installation identity the agent registers with the
// server: an ECDSA P-256 key pair (private key DPAPI-protected on Windows) and
// a stable fingerprint derived from a random installation id. Drive letters and
// card paths never participate in the identity.
package device

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"photography-workflow/agent/internal/auth"
)

const (
	keyIdentity = "pwf-device-key:"
	keyFile     = "device.key"
	metaFile    = "device.json"
)

type meta struct {
	InstallID   string `json:"install_id"`
	DeviceID    string `json:"device_id,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
}

// Identity is the persisted installation identity.
type Identity struct {
	dir     string
	keys    *auth.FileStore
	meta    meta
	private *ecdsa.PrivateKey
}

// Ensure loads or creates the identity at dir. A corrupt private key is
// replaced and the server registration id is cleared so the next heartbeat
// re-registers instead of pointing at a key it no longer owns.
func Ensure(dir, name string) (*Identity, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("设备目录为空")
	}
	id := &Identity{dir: dir, meta: meta{Name: strings.TrimSpace(name)}}
	regen := false

	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &id.meta); err != nil {
			regen = true
		}
	case errors.Is(err, fs.ErrNotExist):
		regen = true
	default:
		return nil, fmt.Errorf("读取设备信息失败：%w", err)
	}
	if id.meta.Name == "" {
		id.meta.Name = strings.TrimSpace(name)
	}
	if id.meta.Name == "" {
		id.meta.Name = "PWF Agent"
	}
	if id.meta.InstallID == "" {
		regen = true
	}
	if regen {
		id.meta.InstallID = uuid.NewString()
		id.meta.DeviceID = ""
		id.meta.Fingerprint = fingerprint(id.meta.InstallID)
	}

	keys, err := auth.NewFileStore(dir, keyIdentity+id.meta.InstallID)
	if err != nil {
		return nil, err
	}
	id.keys = keys

	key, err := loadKey(keys)
	if err != nil || key == nil {
		key, err = generateKey()
		if err != nil {
			return nil, err
		}
		if err := saveKey(keys, key); err != nil {
			return nil, err
		}
		id.meta.DeviceID = ""
	}
	id.private = key
	pub, err := encodePublic(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	if id.meta.PublicKey != pub {
		id.meta.PublicKey = pub
	}
	if err := id.save(); err != nil {
		return nil, err
	}
	return id, nil
}

func fingerprint(installID string) string {
	sum := sha256.Sum256([]byte("pwf-agent|" + installID))
	return "pwf-" + hex.EncodeToString(sum[:16])
}

func generateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

func loadKey(store *auth.FileStore) (*ecdsa.PrivateKey, error) {
	data, err := store.Load()
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("私钥已损坏：%w", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("设备私钥类型不正确")
	}
	return ec, nil
}

func saveKey(store *auth.FileStore, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("编码设备私钥失败：%w", err)
	}
	if err := store.Save(der); err != nil {
		return fmt.Errorf("保存设备私钥失败：%w", err)
	}
	return nil
}

func encodePublic(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("编码设备公钥失败：%w", err)
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

func (id *Identity) save() error {
	if err := os.MkdirAll(id.dir, 0o700); err != nil {
		return fmt.Errorf("创建设备目录失败：%w", err)
	}
	data, err := json.MarshalIndent(id.meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(id.dir, metaFile+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("保存设备信息失败：%w", err)
	}
	if err := os.Rename(tmp, filepath.Join(id.dir, metaFile)); err != nil {
		return fmt.Errorf("保存设备信息失败：%w", err)
	}
	return nil
}

// PrivateKey returns the device key for future device-token signatures.
func (id *Identity) PrivateKey() *ecdsa.PrivateKey { return id.private }

func (id *Identity) PublicKey() string   { return id.meta.PublicKey }
func (id *Identity) Fingerprint() string { return id.meta.Fingerprint }
func (id *Identity) Name() string        { return id.meta.Name }
func (id *Identity) ServerID() string    { return id.meta.DeviceID }

// SetServerID records the id the server assigned at registration.
func (id *Identity) SetServerID(serverID string) error {
	if id.meta.DeviceID == serverID {
		return nil
	}
	id.meta.DeviceID = serverID
	return id.save()
}

// KeyPath reports where the protected private key lives, for diagnostics.
func (id *Identity) KeyPath() string { return id.keys.Path() }
