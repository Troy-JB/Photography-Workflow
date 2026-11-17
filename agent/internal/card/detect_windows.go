//go:build windows

package card

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// Detect lists removable volumes that are present and ready.
func Detect(ctx context.Context) ([]Volume, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("card: list drives: %w", err)
	}
	var out []Volume
	for i := 0; i < 26; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		rootp, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if windows.GetDriveType(rootp) != windows.DRIVE_REMOVABLE {
			continue
		}
		v, err := inspect(rootp)
		if err != nil {
			// An empty card reader reports NOT_READY; skip it silently.
			continue
		}
		v.Root = root
		out = append(out, v)
	}
	return out, nil
}

func inspect(root *uint16) (Volume, error) {
	var label [windows.MAX_PATH + 1]uint16
	var fsName [windows.MAX_PATH + 1]uint16
	var serial, maxComponent, flags uint32
	if err := windows.GetVolumeInformation(root,
		&label[0], uint32(len(label)), &serial, &maxComponent, &flags,
		&fsName[0], uint32(len(fsName))); err != nil {
		return Volume{}, err
	}
	var guid [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(root, &guid[0], uint32(len(guid))); err != nil {
		return Volume{}, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(root, &free, &total, &totalFree); err != nil {
		total = 0
	}
	return Volume{
		GUID:     windows.UTF16ToString(guid[:]),
		Label:    windows.UTF16ToString(label[:]),
		Serial:   serial,
		Capacity: total,
	}, nil
}

// MountPoint returns the first mount point of a volume GUID path.
func MountPoint(guid string) (string, error) {
	if guid == "" {
		return "", errors.New("card: volume GUID is empty")
	}
	gp, err := windows.UTF16PtrFromString(guid)
	if err != nil {
		return "", err
	}
	var buf [windows.MAX_PATH + 1]uint16
	var returned uint32
	if err := windows.GetVolumePathNamesForVolumeName(gp, &buf[0], uint32(len(buf)), &returned); err != nil {
		return "", err
	}
	if returned == 0 {
		return "", errors.New("card: volume is not mounted")
	}
	// The buffer is a MULTI_SZ list; the first string is the primary mount.
	name := windows.UTF16ToString(buf[:])
	if name == "" {
		return "", errors.New("card: volume has no mount point")
	}
	return name, nil
}
