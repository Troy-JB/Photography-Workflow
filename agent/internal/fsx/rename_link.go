//go:build !windows

package fsx

import (
	"errors"
	"fmt"
	"os"
)

// linkRename publishes oldPath at newPath only if newPath is free: link fails
// atomically with EEXIST, and the temporary name is removed afterwards.
func linkRename(oldPath, newPath string) error {
	if err := os.Link(oldPath, newPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExist, newPath)
		}
		return &os.LinkError{Op: "link", Old: oldPath, New: newPath, Err: err}
	}
	// The final file is in place; a leftover temp name is recoverable and must
	// not fail the copy.
	_ = os.Remove(oldPath)
	return nil
}
