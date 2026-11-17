// Package settings persists operator preferences as a small JSON file next to
// the state database. Writes are atomic so a crash never leaves a torn file.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"photography-workflow/agent/internal/api"
)

const (
	minWorkers = 1
	maxWorkers = 8
	fileMode   = 0o600
	// maxBandwidthKbps caps a value that is almost certainly a typo (10 Gbps).
	maxBandwidthKbps = 10_000_000
)

// Settings is the on-disk configuration. DestB is kept even when RequireBackup
// is false so re-enabling backup does not lose the previous path. Tool paths
// are optional overrides; the installer's bundled tools are found first.
//
// ServerURL is the tenant's API root. TenantSlug is only needed when one email
// belongs to several tenants; leave it empty otherwise. BandwidthKbps of zero
// means unlimited upload bandwidth.
type Settings struct {
	DestA         string `json:"dest_a"`
	DestB         string `json:"dest_b"`
	RequireBackup bool   `json:"require_backup"`
	Workers       int    `json:"workers"`
	ProjectCode   string `json:"project_code"`
	ExifToolPath  string `json:"exiftool_path,omitempty"`
	FFmpegPath    string `json:"ffmpeg_path,omitempty"`
	ServerURL     string `json:"server_url,omitempty"`
	TenantSlug    string `json:"tenant_slug,omitempty"`
	BandwidthKbps int    `json:"bandwidth_kbps,omitempty"`
}

// Default is used on first run: dual destination, two copy workers.
func Default() Settings {
	return Settings{RequireBackup: true, Workers: 2}
}

// Load reads the file at path. A missing file returns Default so a fresh
// install starts with backup enabled; a corrupt file is an error, never
// silently replaced.
func Load(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Default(), nil
		}
		return Settings{}, fmt.Errorf("settings: read %s: %w", path, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("settings: parse %s: %w", path, err)
	}
	return s.normalized(), nil
}

// Save validates and atomically replaces the file at path.
func Save(path string, s Settings) error {
	s = s.normalized()
	if err := s.Validate(); err != nil {
		return err
	}
	return write(path, s)
}

// SaveConnection persists only the server connection fields. The server screen
// uses it so an operator can sign in before choosing destination folders; the
// import screen still refuses to run without them.
func SaveConnection(path, serverURL, tenantSlug string, bandwidthKbps int) (Settings, error) {
	s, err := Load(path)
	if err != nil {
		return Settings{}, err
	}
	s.ServerURL = serverURL
	s.TenantSlug = tenantSlug
	s.BandwidthKbps = bandwidthKbps
	s = s.normalized()
	if err := s.ValidateServer(); err != nil {
		return Settings{}, err
	}
	if err := write(path, s); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func write(path string, s Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("settings: create directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: encode: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return fmt.Errorf("settings: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settings: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settings: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("settings: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("settings: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("settings: replace %s: %w", path, err)
	}
	return nil
}

// Validate checks the combinations the importer cannot recover from. Path
// existence and write probes belong to the import screen (destinations move).
func (s Settings) Validate() error {
	a, b := strings.TrimSpace(s.DestA), strings.TrimSpace(s.DestB)
	if s.RequireBackup {
		if a == "" {
			return errors.New("settings: destination A is required")
		}
		if b == "" {
			return errors.New("settings: backup is enabled but destination B is empty")
		}
	}
	if a != "" && b != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return errors.New("settings: destinations A and B must differ")
	}
	if s.Workers < minWorkers || s.Workers > maxWorkers {
		return fmt.Errorf("settings: workers must be between %d and %d", minWorkers, maxWorkers)
	}
	return s.ValidateServer()
}

// ValidateServer checks only the connection fields, so the server screen can
// save a URL before destinations exist.
func (s Settings) ValidateServer() error {
	if s.ServerURL != "" {
		if _, err := api.ParseServerURL(s.ServerURL); err != nil {
			return fmt.Errorf("settings: %w", err)
		}
	}
	if s.TenantSlug != "" && s.ServerURL == "" {
		return errors.New("settings: tenant slug requires a server URL")
	}
	if s.BandwidthKbps < 0 {
		return errors.New("settings: bandwidth must not be negative")
	}
	if s.BandwidthKbps > maxBandwidthKbps {
		return fmt.Errorf("settings: bandwidth must not exceed %d kbps", maxBandwidthKbps)
	}
	return nil
}

func (s Settings) normalized() Settings {
	s.DestA = strings.TrimSpace(s.DestA)
	s.DestB = strings.TrimSpace(s.DestB)
	s.ProjectCode = strings.TrimSpace(s.ProjectCode)
	s.ExifToolPath = strings.TrimSpace(s.ExifToolPath)
	s.FFmpegPath = strings.TrimSpace(s.FFmpegPath)
	s.ServerURL = strings.TrimSpace(s.ServerURL)
	s.TenantSlug = strings.TrimSpace(s.TenantSlug)
	if u, err := api.ParseServerURL(s.ServerURL); err == nil {
		s.ServerURL = u.String()
	}
	if s.Workers == 0 {
		s.Workers = 2
	}
	if s.Workers < minWorkers {
		s.Workers = minWorkers
	}
	if s.Workers > maxWorkers {
		s.Workers = maxWorkers
	}
	return s
}
