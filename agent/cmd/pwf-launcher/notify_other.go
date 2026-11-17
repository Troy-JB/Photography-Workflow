//go:build !windows

package main

import (
	"fmt"
	"os"
)

func notifyError(msg string) { fmt.Fprintln(os.Stderr, msg) }
