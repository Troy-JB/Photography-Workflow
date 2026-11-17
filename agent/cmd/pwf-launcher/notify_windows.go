//go:build windows

package main

import "golang.org/x/sys/windows"

func notifyError(msg string) {
	title, _ := windows.UTF16PtrFromString("PWF Agent")
	text, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, text, title, windows.MB_OK|windows.MB_ICONERROR)
}
