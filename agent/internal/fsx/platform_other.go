//go:build !windows && !linux

package fsx

import (
	"errors"
	"os"
	"syscall"
)

func Local(p string) string { return p }

func isCaseInsensitiveFS() bool { return false }

func RenameNoReplace(oldPath, newPath string) error {
	return linkRename(oldPath, newPath)
}

func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func FreeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, &os.PathError{Op: "freespace", Path: path, Err: err}
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func sourceReason(err error) Reason {
	if errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENODEV) {
		return ReasonCardRemoved
	}
	return ""
}

func destinationReason(err error) Reason {
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return ReasonDiskFull
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EROFS):
		return ReasonDestination
	}
	return ""
}
