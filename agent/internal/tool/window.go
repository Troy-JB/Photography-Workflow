package tool

import (
	"os/exec"
	"runtime"
)

// HideWindow stops console helpers (exiftool, ffmpeg) from flashing a console
// window when the agent runs as a GUI. It is a no-op outside Windows.
func HideWindow(cmd *exec.Cmd) {
	if runtime.GOOS == "windows" {
		hideWindow(cmd)
	}
}
