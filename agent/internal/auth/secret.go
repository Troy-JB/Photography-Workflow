package auth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func joinSecret(dir, name string) string { return filepath.Join(dir, name) }

func readSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read secret: %w", err)
	}
	return data, nil
}

// writeSecret replaces the file atomically and never leaves a readable partial
// blob behind.
func writeSecret(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secret directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".secret-*.tmp")
	if err != nil {
		return fmt.Errorf("create secret temp: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod secret: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write secret: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync secret: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close secret: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace secret: %w", err)
	}
	return nil
}

func removeSecret(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove secret: %w", err)
	}
	return nil
}
