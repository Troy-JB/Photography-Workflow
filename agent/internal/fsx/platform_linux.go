//go:build linux

package fsx

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func Local(p string) string { return p }

func isCaseInsensitiveFS() bool { return false }

// RenameNoReplace uses RENAME_NOREPLACE so an existing destination surfaces as
// ErrExist. Filesystems without support fall back to link+unlink, which is
// still atomic against an existing target.
func RenameNoReplace(oldPath, newPath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%w: %s", ErrExist, newPath)
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTSUP):
		return linkRename(oldPath, newPath)
	}
	return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: err}
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
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, &os.PathError{Op: "freespace", Path: path, Err: err}
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func sourceReason(err error) Reason {
	if errors.Is(err, unix.ENODEV) {
		return ReasonCardRemoved
	}
	return ""
}

func destinationReason(err error) Reason {
	switch {
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		return ReasonDiskFull
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
		return ReasonDestination
	}
	return ""
}
