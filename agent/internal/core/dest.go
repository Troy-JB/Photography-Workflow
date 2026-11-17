package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"photography-workflow/agent/internal/copy"
	"photography-workflow/agent/internal/fsx"
	"photography-workflow/agent/internal/model"
)

// ValidateDestinations rejects configurations that could overwrite the card or
// write both copies to the same place.
func ValidateDestinations(destA, destB, sourceRoot string, requireBackup bool) error {
	if err := validateDest("A", destA, sourceRoot); err != nil {
		return err
	}
	if !requireBackup {
		return nil
	}
	if err := validateDest("B", destB, sourceRoot); err != nil {
		return err
	}
	if fsx.SamePath(destA, destB) {
		return errors.New("destinations A and B must be different directories")
	}
	if fsx.Within(destA, destB) || fsx.Within(destB, destA) {
		return errors.New("destinations must not nest inside each other")
	}
	return nil
}

func validateDest(name, dest, sourceRoot string) error {
	if strings.TrimSpace(dest) == "" {
		return fmt.Errorf("destination %s is not configured", name)
	}
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("destination %s must be an absolute path", name)
	}
	info, err := os.Stat(fsx.Local(dest))
	if err != nil {
		return fmt.Errorf("destination %s is not accessible: %w", name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("destination %s is not a directory", name)
	}
	if sourceRoot != "" && fsx.Within(sourceRoot, dest) {
		return fmt.Errorf("destination %s must not be inside the card", name)
	}
	return nil
}

// ProbeWritable verifies a destination accepts a small file. It catches
// read-only media, missing UNC shares and over-restrictive ACLs before any
// card data is read.
func ProbeWritable(dir string) error {
	f, err := os.CreateTemp(fsx.Local(dir), ".pwf-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write([]byte("pwf-probe"))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if rerr := os.Remove(name); werr == nil && rerr != nil {
		werr = rerr
	}
	return werr
}

// destinations builds the enabled destination list for an import.
func destinations(im *model.Import) []copy.Dest {
	out := []copy.Dest{{Name: model.DestA, Root: im.DestA}}
	if im.RequireBackup && im.DestB != "" {
		out = append(out, copy.Dest{Name: model.DestB, Root: im.DestB})
	}
	return out
}
