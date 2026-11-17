// Command pwf-launcher starts the version named by current.txt in the
// install root. Shortcuts point here so a rollback or update can switch
// versions without rewriting links.
package main

import (
	"os"
	"os/exec"
	"path/filepath"

	"photography-workflow/agent/internal/updater"
)

func main() {
	exe, err := os.Executable()
	if err != nil {
		notifyError("无法定位启动器：" + err.Error())
		os.Exit(1)
	}
	root := filepath.Dir(exe)
	target, err := updater.Target(root)
	if err != nil {
		notifyError("PWF Agent 无法启动：" + err.Error())
		os.Exit(1)
	}
	cmd := exec.Command(target, os.Args[1:]...)
	cmd.Dir = filepath.Dir(target)
	if err := cmd.Start(); err != nil {
		notifyError("PWF Agent 启动失败：" + err.Error())
		os.Exit(1)
	}
}
