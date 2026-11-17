// Package tool locates the external helpers the agent ships with. The
// installer places them under <app>\tools, but an operator can point at an
// existing installation from settings; PATH is the last resort.
package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Paths holds the resolved helper binaries. An empty field means the tool was
// not found; callers decide whether that is fatal.
type Paths struct {
	ExifTool string
	FFmpeg   string
}

func (p Paths) HasExifTool() bool { return p.ExifTool != "" }
func (p Paths) HasFFmpeg() bool   { return p.FFmpeg != "" }

// Locate resolves both helpers. Explicit settings win, then the bundled tools
// directory next to the executable, then PATH.
func Locate(exifTool, ffmpeg string) Paths {
	return Paths{
		ExifTool: resolve(exifTool, "exiftool"),
		FFmpeg:   resolve(ffmpeg, "ffmpeg"),
	}
}

func resolve(explicit, name string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			if abs, err := filepath.Abs(explicit); err == nil {
				return abs
			}
			return explicit
		}
	}
	for _, candidate := range bundledCandidates(name) {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if path, err := exec.LookPath(executableName(name)); err == nil {
		return path
	}
	return ""
}

func bundledCandidates(name string) []string {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	dir := filepath.Dir(exe)
	names := []string{executableName(name)}
	if runtime.GOOS == "windows" {
		// ExifTool's Windows distribution is a single exe; ffmpeg builds vary.
		names = append(names, name+".exe")
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(dir, "tools", n))
	}
	return out
}

func executableName(name string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return name + ".exe"
	}
	return name
}

// ExifToolVersion runs `exiftool -ver`. It proves the binary starts and is
// the expected tool before an import depends on it.
func ExifToolVersion(ctx context.Context, path string) (string, error) {
	return oneShot(ctx, path, "-ver")
}

// FFmpegVersion runs `ffmpeg -version` and returns its first line.
func FFmpegVersion(ctx context.Context, path string) (string, error) {
	out, err := oneShot(ctx, path, "-version")
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(line), nil
}

func oneShot(parent context.Context, path string, args ...string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("tool: binary not configured")
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tool: run %s: %w", filepath.Base(path), err)
	}
	return strings.TrimSpace(string(out)), nil
}
