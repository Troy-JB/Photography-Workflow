//go:build windows

package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Local adapts an absolute path for Win32 long-path APIs. Relative paths are
// returned untouched: they are only used for display, never for syscalls.
func Local(p string) string {
	if p == "" || strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return p
	}
	if !filepath.IsAbs(p) {
		return p
	}
	clean := filepath.Clean(p)
	if strings.HasPrefix(clean, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(clean, `\\`)
	}
	return `\\?\` + clean
}

func isCaseInsensitiveFS() bool { return true }

// RenameNoReplace moves a file and fails with ErrExist instead of replacing an
// existing destination. MOVEFILE_WRITE_THROUGH flushes the metadata change.
func RenameNoReplace(oldPath, newPath string) error {
	from, err := windows.UTF16PtrFromString(Local(oldPath))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: err}
	}
	to, err := windows.UTF16PtrFromString(Local(newPath))
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: err}
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return fmt.Errorf("%w: %s", ErrExist, newPath)
		}
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: err}
	}
	return nil
}

// SyncDir is a no-op: Windows cannot fsync a directory handle. The rename
// above uses WRITE_THROUGH, which covers the durability the copy engine needs.
func SyncDir(dir string) error { return nil }

func FreeSpace(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(Local(path))
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, &os.PathError{Op: "freespace", Path: path, Err: err}
	}
	return free, nil
}

func sourceReason(err error) Reason {
	switch {
	case errors.Is(err, windows.ERROR_NOT_READY),
		errors.Is(err, windows.ERROR_DEVICE_NOT_CONNECTED),
		errors.Is(err, windows.ERROR_UNRECOGNIZED_MEDIA),
		errors.Is(err, windows.ERROR_DEVICE_REMOVED):
		return ReasonCardRemoved
	}
	return ""
}

func destinationReason(err error) Reason {
	switch {
	case errors.Is(err, windows.ERROR_DISK_FULL),
		errors.Is(err, windows.ERROR_HANDLE_DISK_FULL):
		return ReasonDiskFull
	case errors.Is(err, windows.ERROR_ACCESS_DENIED),
		errors.Is(err, windows.ERROR_WRITE_PROTECT),
		errors.Is(err, windows.ERROR_FILENAME_EXCED_RANGE),
		errors.Is(err, windows.ERROR_INVALID_NAME),
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return ReasonDestination
	}
	return ""
}
