//go:build !windows

package card

import (
	"context"
	"errors"
)

// Detect is a no-op off Windows; the CLI and CI use directory sources.
func Detect(context.Context) ([]Volume, error) { return nil, nil }

func MountPoint(string) (string, error) {
	return "", errors.New("card: volume detection is only available on Windows")
}
