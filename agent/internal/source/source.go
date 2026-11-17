// Package source abstracts where card content is read from: a plain directory
// (manual selection, tests, fixed disks) or a mounted camera volume.
package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"photography-workflow/agent/internal/fsx"
)

// Source is a read-only view of card content addressed by slash-separated
// relative paths, plus a liveness probe used to detect card removal.
type Source interface {
	Root() string
	FS() fs.FS
	Alive() error
}

type Dir struct {
	root string
	fsys fs.FS
}

// NewDir validates that root exists and is a directory.
func NewDir(root string) (*Dir, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(fsx.Local(abs))
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("source: not a directory")
	}
	return &Dir{root: abs, fsys: localFS{root: abs}}, nil
}

func (d *Dir) Root() string { return d.root }

func (d *Dir) FS() fs.FS { return d.fsys }

// Alive checks that the directory is still reachable, which is how a removed
// volume usually surfaces even when the drive letter is reused.
func (d *Dir) Alive() error {
	info, err := os.Stat(fsx.Local(d.root))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("source: root is no longer a directory")
	}
	return nil
}

// localFS applies fsx.Local at every syscall so long and UNC roots work.
type localFS struct {
	root string
}

func (l localFS) full(name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return l.root, nil
	}
	return filepath.Join(l.root, filepath.FromSlash(name)), nil
}

func (l localFS) Open(name string) (fs.File, error) {
	full, err := l.full(name)
	if err != nil {
		return nil, err
	}
	return os.Open(fsx.Local(full))
}

func (l localFS) Stat(name string) (fs.FileInfo, error) {
	full, err := l.full(name)
	if err != nil {
		return nil, err
	}
	return os.Stat(fsx.Local(full))
}

func (l localFS) ReadDir(name string) ([]fs.DirEntry, error) {
	full, err := l.full(name)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(fsx.Local(full))
}

func (l localFS) ReadFile(name string) ([]byte, error) {
	full, err := l.full(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(fsx.Local(full))
}
