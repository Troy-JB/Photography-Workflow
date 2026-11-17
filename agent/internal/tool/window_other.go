//go:build !windows

package tool

import "os/exec"

func hideWindow(*exec.Cmd) {}
