// Package copy implements the agent's copy phase: read the card once, tee the
// stream into every enabled destination, fsync both temps, re-hash them and
// only then publish them under their final names with a no-replace rename.
package copy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"photography-workflow/agent/internal/fsx"
	contenthash "photography-workflow/agent/internal/hash"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/source"
)

// Dest is one enabled local destination (A, or A and B).
type Dest struct {
	Name string
	Root string
}

// Layout maps a card-relative path into the operator's archive structure:
// <root>/<project_code>/<shoot_date>/<card_label>/<rel_path>.
type Layout struct {
	ProjectCode string
	ShootDate   string
	CardLabel   string
}

func (l Layout) Merge(destRoot, rel string) (string, error) {
	rel, err := fsx.CleanRel(rel)
	if err != nil {
		return "", err
	}
	base := filepath.Join(destRoot,
		fsx.SanitizeSegment(l.ProjectCode, "project"),
		fsx.SanitizeSegment(l.ShootDate, "undated"),
		fsx.SanitizeSegment(l.CardLabel, "card"))
	final := filepath.Join(base, filepath.FromSlash(rel))
	if !fsx.Within(destRoot, final) {
		return "", fmt.Errorf("copy: %q escapes destination root", rel)
	}
	return final, nil
}

// Store is the slice of the local database the engine needs.
type Store interface {
	Copies(ctx context.Context, fileID string) ([]model.Copy, error)
	SaveCopy(ctx context.Context, c model.Copy) error
	SetFileState(ctx context.Context, id string, from []model.FileState, to model.FileState, errMsg string) (bool, error)
	FinishLocalMove(ctx context.Context, id string, from []model.FileState, to model.FileState, sha, errMsg string) (bool, error)
}

// StopError means the whole import must pause, not just this file.
type StopError struct {
	Reason fsx.Reason
	Err    error
}

func (e *StopError) Error() string {
	return fmt.Sprintf("copy paused (%s): %v", e.Reason, e.Err)
}

func (e *StopError) Unwrap() error { return e.Err }

type Engine struct {
	Store      Store
	Dests      []Dest
	BufferSize int
}

var claimable = []model.FileState{
	model.StateDiscovered, model.StateCopyFailed, model.StateHashMismatch,
}

type plan struct {
	dest      Dest
	final     string
	row       model.Copy
	skip      bool
	verified  bool
	adoptPath string
	adoptHash string
	tempPath  string
	tempFile  *os.File
}

// RunFile copies one file to every enabled destination and advances it to
// local_verified only when all copies re-hash to the source SHA-256.
func (e *Engine) RunFile(ctx context.Context, src source.Source, f model.File, layout Layout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(e.Dests) == 0 {
		return errors.New("copy: no destinations configured")
	}
	claimed, err := e.Store.SetFileState(ctx, f.ID, claimable, model.StateCopying, "")
	if err != nil {
		return fmt.Errorf("copy: claim file %s: %w", f.ID, err)
	}
	if !claimed {
		// Another worker owns it or it is already local.
		return nil
	}

	var plans []*plan
	abort := func(reason fsx.Reason, state model.FileState, msg string, cause error) error {
		cleanupTemps(plans)
		if merr := e.markCopiesFailed(ctx, plans, msg); merr != nil && cause == nil {
			cause = merr
		}
		if _, serr := e.Store.SetFileState(ctx, f.ID, []model.FileState{model.StateCopying}, state, msg); serr != nil && cause == nil {
			cause = serr
		}
		if reason != "" {
			return &StopError{Reason: reason, Err: fmt.Errorf("%s: %w", msg, cause)}
		}
		return fmt.Errorf("%s: %w", msg, cause)
	}

	if err := src.Alive(); err != nil {
		return abort(sourceReason(err), model.StateCopyFailed, "card is no longer available", err)
	}

	existing, err := e.Store.Copies(ctx, f.ID)
	if err != nil {
		return abort("", model.StateCopyFailed, "load copy state", err)
	}
	byDest := make(map[string]model.Copy, len(existing))
	for _, c := range existing {
		byDest[c.Dest] = c
	}

	for _, d := range e.Dests {
		p := &plan{dest: d, row: byDest[d.Name]}
		final := p.row.FinalPath
		if final == "" {
			final, err = layout.Merge(d.Root, f.RelPath)
			if err != nil {
				return abort("", model.StateCopyFailed, fmt.Sprintf("invalid relative path %q", f.RelPath), err)
			}
		}
		p.final = final

		if p.row.State == model.CopyVerified && p.row.FinalPath != "" {
			ok, err := fsx.Exists(p.row.FinalPath)
			if err != nil {
				return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
					fmt.Sprintf("check verified copy on %s", d.Name), err)
			}
			if ok {
				p.skip = true
				p.verified = true
				plans = append(plans, p)
				continue
			}
		}

		adopt := p.row.FinalPath
		if adopt == "" {
			adopt = final
		}
		ok, err := fsx.Exists(adopt)
		if err != nil {
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("inspect destination %s", d.Name), err)
		}
		if ok {
			p.adoptPath = adopt
		}
		p.row = model.Copy{FileID: f.ID, Dest: d.Name, FinalPath: final, State: model.CopyPending}
		plans = append(plans, p)
	}

	needWork := false
	for _, p := range plans {
		if !p.skip {
			needWork = true
			break
		}
	}
	if !needWork {
		sha := plans[0].row.SHA256
		if sha == "" {
			res, err := contenthash.File(ctx, plans[0].row.FinalPath)
			if err != nil {
				return abort(fsx.ClassifyDestination(err), model.StateCopyFailed, "hash verified copy", err)
			}
			sha = res.SHA256
		}
		return e.finishLocal(ctx, f.ID, sha)
	}

	// Hash files already sitting at their final names before touching the
	// card, so a resumed or re-run import can adopt them instead of copying.
	for _, p := range plans {
		if p.skip || p.adoptPath == "" {
			continue
		}
		res, err := contenthash.File(ctx, p.adoptPath)
		if err != nil {
			if r := fsx.ClassifyDestination(err); r != "" {
				return abort(r, model.StateCopyFailed, fmt.Sprintf("read existing file on %s", p.dest.Name), err)
			}
			p.adoptPath = ""
			continue
		}
		p.adoptHash = res.SHA256
	}

	short := shortID(f.ID)
	for _, p := range plans {
		if p.skip {
			continue
		}
		dir := filepath.Dir(p.final)
		if err := fsx.EnsureDir(dir); err != nil {
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("create directory on %s", p.dest.Name), err)
		}
		tempPath := filepath.Join(dir, fmt.Sprintf(".pwf-%s-%s.part", short, uuid.NewString()))
		tf, err := os.OpenFile(fsx.Local(tempPath), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("create temporary file on %s", p.dest.Name), err)
		}
		p.tempPath = tempPath
		p.tempFile = tf
		p.row = model.Copy{
			FileID: f.ID, Dest: p.dest.Name, FinalPath: p.final,
			TempPath: tempPath, State: model.CopyCopying,
		}
		// Persist the intent so a crash leaves a temp path the recovery path
		// can clean up.
		if err := e.Store.SaveCopy(ctx, p.row); err != nil {
			return err
		}
	}

	srcFile, err := src.FS().Open(f.RelPath)
	if err != nil {
		if r := fsx.ClassifySource(err); r != "" {
			return abort(r, model.StateCopyFailed, "open source file", err)
		}
		if aerr := src.Alive(); aerr != nil {
			return abort(sourceReason(aerr), model.StateCopyFailed, "card is no longer available", aerr)
		}
		return abort("", model.StateCopyFailed, fmt.Sprintf("open source %q", f.RelPath), err)
	}
	defer srcFile.Close()

	info, err := srcFile.Stat()
	if err != nil {
		return abort("", model.StateCopyFailed, "stat source file", err)
	}
	if info.Size() != f.Size {
		return abort("", model.StateCopyFailed,
			fmt.Sprintf("source size changed since scan: %d != %d", info.Size(), f.Size), nil)
	}

	hasher := contenthash.New()
	buf := make([]byte, e.bufferSize())
	for {
		if err := ctx.Err(); err != nil {
			return abort("", model.StateCopyFailed, "import cancelled", err)
		}
		n, rerr := srcFile.Read(buf)
		if n > 0 {
			if _, err := hasher.Write(buf[:n]); err != nil {
				return abort("", model.StateCopyFailed, "hash source stream", err)
			}
			for _, p := range plans {
				if p.tempFile == nil {
					continue
				}
				if _, werr := p.tempFile.Write(buf[:n]); werr != nil {
					msg := fmt.Sprintf("write copy on %s", p.dest.Name)
					return abort(fsx.ClassifyDestination(werr), model.StateCopyFailed, msg, werr)
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if r := fsx.ClassifySource(rerr); r != "" {
				return abort(r, model.StateCopyFailed, "read card", rerr)
			}
			if aerr := src.Alive(); aerr != nil {
				return abort(sourceReason(aerr), model.StateCopyFailed, "card is no longer available", aerr)
			}
			return abort("", model.StateCopyFailed, fmt.Sprintf("read source %q", f.RelPath), rerr)
		}
	}

	sourceSHA := hasher.SHA256()
	if hasher.Bytes() != f.Size {
		return abort("", model.StateCopyFailed,
			fmt.Sprintf("source read %d bytes, expected %d", hasher.Bytes(), f.Size), nil)
	}

	for _, p := range plans {
		if p.tempFile == nil {
			continue
		}
		if err := p.tempFile.Sync(); err != nil {
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("flush temporary file on %s", p.dest.Name), err)
		}
		if err := p.tempFile.Close(); err != nil {
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("close temporary file on %s", p.dest.Name), err)
		}
		p.tempFile = nil
	}

	for _, p := range plans {
		if p.skip {
			continue
		}
		if p.adoptHash != "" && p.adoptHash == sourceSHA {
			_ = fsx.RemoveIfExists(p.tempPath)
			p.tempPath = ""
			p.row = model.Copy{
				FileID: f.ID, Dest: p.dest.Name, FinalPath: p.adoptPath,
				State: model.CopyVerified, Bytes: f.Size, SHA256: sourceSHA,
			}
			if err := e.Store.SaveCopy(ctx, p.row); err != nil {
				return err
			}
			p.verified = true
			continue
		}
		if p.tempPath == "" {
			return abort("", model.StateCopyFailed, "no temporary copy to verify", nil)
		}

		res, err := contenthash.File(ctx, p.tempPath)
		if err != nil {
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("verify temporary copy on %s", p.dest.Name), err)
		}
		if res.SHA256 != sourceSHA || res.Bytes != f.Size {
			return abort("", model.StateHashMismatch,
				fmt.Sprintf("copy on %s hashes to %s (%d bytes)", p.dest.Name, res.SHA256, res.Bytes), nil)
		}

		target := p.final
		if p.adoptPath != "" {
			// The computed final name is taken by a different file; never
			// delete or overwrite it.
			target = altPath(p.final, 2)
		}
		placed := false
		for attempt := 0; attempt < 100; attempt++ {
			err := fsx.RenameNoReplace(p.tempPath, target)
			if err == nil {
				placed = true
				break
			}
			if errors.Is(err, fsx.ErrExist) {
				res, herr := contenthash.File(ctx, target)
				if herr == nil && res.SHA256 == sourceSHA && res.Bytes == f.Size {
					_ = fsx.RemoveIfExists(p.tempPath)
					placed = true
					break
				}
				target = altPath(p.final, attempt+2)
				continue
			}
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("publish copy on %s", p.dest.Name), err)
		}
		if !placed {
			return abort("", model.StateCopyFailed,
				fmt.Sprintf("no free destination name on %s", p.dest.Name), nil)
		}
		if err := fsx.SyncDir(filepath.Dir(target)); err != nil {
			return abort(fsx.ClassifyDestination(err), model.StateCopyFailed,
				fmt.Sprintf("sync directory on %s", p.dest.Name), err)
		}
		p.tempPath = ""
		p.row = model.Copy{
			FileID: f.ID, Dest: p.dest.Name, FinalPath: target,
			State: model.CopyVerified, Bytes: f.Size, SHA256: sourceSHA,
		}
		if err := e.Store.SaveCopy(ctx, p.row); err != nil {
			return err
		}
		p.verified = true
	}

	return e.finishLocal(ctx, f.ID, sourceSHA)
}

func (e *Engine) finishLocal(ctx context.Context, fileID, sha string) error {
	ok, err := e.Store.FinishLocalMove(ctx, fileID,
		[]model.FileState{model.StateCopying}, model.StateLocalVerified, sha, "")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("copy: file %s changed state during copy", fileID)
	}
	return nil
}

func (e *Engine) markCopiesFailed(ctx context.Context, plans []*plan, msg string) error {
	var firstErr error
	for _, p := range plans {
		if p.skip || p.verified {
			continue
		}
		p.row.State = model.CopyFailed
		p.row.TempPath = ""
		p.row.Error = msg
		if err := e.Store.SaveCopy(ctx, p.row); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func cleanupTemps(plans []*plan) {
	for _, p := range plans {
		if p.tempFile != nil {
			_ = p.tempFile.Close()
			p.tempFile = nil
		}
		if p.tempPath != "" {
			_ = fsx.RemoveIfExists(p.tempPath)
			p.tempPath = ""
		}
	}
}

func (e *Engine) bufferSize() int {
	if e.BufferSize > 0 {
		return e.BufferSize
	}
	return contenthash.BufferSize
}

func sourceReason(err error) fsx.Reason {
	if r := fsx.ClassifySource(err); r != "" {
		return r
	}
	// A card root that cannot be stat'ed is treated as removed: the import
	// must pause rather than fail every remaining file.
	return fsx.ReasonCardRemoved
}

func altPath(final string, n int) string {
	ext := filepath.Ext(final)
	return strings.TrimSuffix(final, ext) + fmt.Sprintf("__%d", n) + ext
}

func shortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "file"
	}
	return id
}
