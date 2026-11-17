//go:build windows

package card

import (
	"context"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestMountPointRoundTrip(t *testing.T) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		t.Skip(err)
	}
	root := filepath.VolumeName(cwd) + `\`
	rootp, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Skip(err)
	}
	var guid [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(rootp, &guid[0], uint32(len(guid))); err != nil {
		t.Skipf("volume name unavailable: %v", err)
	}
	got, err := MountPoint(windows.UTF16ToString(guid[:]))
	if err != nil {
		t.Fatalf("mount point: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(root) {
		t.Fatalf("mount point = %q, want %q", got, root)
	}
}

func TestDetectDoesNotFail(t *testing.T) {
	if _, err := Detect(context.Background()); err != nil {
		t.Fatalf("detect: %v", err)
	}
}
