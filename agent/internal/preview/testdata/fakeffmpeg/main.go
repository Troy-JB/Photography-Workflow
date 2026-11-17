// Command fakeffmpeg emits a canned frame for preview tests.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println("ffmpeg version fake")
		return
	}
	if os.Getenv("FAKE_FFMPEG_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "fake ffmpeg failure")
		os.Exit(2)
	}
	path := os.Getenv("FAKE_FFMPEG_FRAME")
	if path == "" {
		fmt.Fprintln(os.Stderr, "FAKE_FFMPEG_FRAME not set")
		os.Exit(3)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	_, _ = os.Stdout.Write(data)
}
