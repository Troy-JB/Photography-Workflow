// Package fsx wraps the OS filesystem details the agent depends on: Windows
// long paths, no-replace atomic renames, free-space probing and pause-worthy
// error classification. Paths stored in the database stay in their natural
// form; only the syscall boundary uses the platform adaptation.
package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	ErrInvalidPath = errors.New("fsx: invalid relative path")
	// ErrExist is returned by RenameNoReplace when the destination exists.
	ErrExist = errors.New("fsx: destination exists")
)

var invalidSegmentChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// CleanRel validates a card-relative path used as a filesystem key. It accepts
// slash- or backslash-separated relative paths and rejects anything that could
// escape the source root.
func CleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, `\`, "/")
	if p == "" || p == "." || strings.HasPrefix(p, "/") {
		return "", ErrInvalidPath
	}
	// Reject Windows drive-relative and drive-absolute forms ("C:x", "C:/x");
	// card content is always addressed relative to the card root.
	if len(p) >= 2 && p[1] == ':' {
		return "", ErrInvalidPath
	}
	if !fs.ValidPath(p) {
		return "", ErrInvalidPath
	}
	return p, nil
}

// SanitizeSegment turns an operator-supplied value (project code, shoot date,
// card label) into a safe single path segment.
func SanitizeSegment(s, fallback string) string {
	s = strings.TrimSpace(s)
	s = invalidSegmentChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, " .")
	if s == "" {
		s = fallback
	}
	if windowsReserved[strings.ToUpper(baseName(s))] {
		s = "_" + s
	}
	return truncateUTF8(s, 80)
}

func baseName(s string) string {
	if i := strings.IndexByte(s, '.'); i > 0 {
		return s[:i]
	}
	return s
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// EnsureDir creates a directory (and parents), applying the platform path
// adaptation for deep and UNC paths.
func EnsureDir(path string) error {
	if err := os.MkdirAll(Local(path), 0o755); err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

// Exists reports whether path exists. It is not an error for it not to exist.
func Exists(path string) (bool, error) {
	_, err := os.Stat(Local(path))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// RemoveIfExists deletes a file, tolerating a missing target.
func RemoveIfExists(path string) error {
	err := os.Remove(Local(path))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// SamePath compares two cleaned paths, case-insensitively on Windows.
func SamePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if isCaseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Within reports whether child is base or lives under base.
func Within(base, child string) bool {
	base = filepath.Clean(base)
	child = filepath.Clean(child)
	if SamePath(base, child) {
		return true
	}
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
