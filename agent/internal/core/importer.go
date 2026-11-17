// Package core orchestrates a local import: create it, copy every file to all
// enabled destinations, and pause (never silently degrade) when the card, a
// destination or free space stops the run.
package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"photography-workflow/agent/internal/copy"
	"photography-workflow/agent/internal/fsx"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/scan"
	"photography-workflow/agent/internal/source"
	"photography-workflow/agent/internal/store"
)

var (
	ErrImportExists = errors.New("core: card already has an active import")
	ErrEmptySource  = errors.New("core: source contains no files")
)

const minHeadroomBytes = 1 << 30 // 1 GiB safety margin beyond 10% of the batch.

// SourceResolver finds the mount point for an import's card. The default reads
// the directory recorded at import time; volume-based resolvers map the card's
// volume GUID to its current mount point.
type SourceResolver interface {
	Resolve(ctx context.Context, imp *model.Import) (source.Source, error)
}

type ResolverFunc func(ctx context.Context, imp *model.Import) (source.Source, error)

func (f ResolverFunc) Resolve(ctx context.Context, imp *model.Import) (source.Source, error) {
	return f(ctx, imp)
}

type dirResolver struct{}

func (dirResolver) Resolve(_ context.Context, imp *model.Import) (source.Source, error) {
	return source.NewDir(imp.SourceRoot)
}

type Options struct {
	Workers   int
	Resolver  SourceResolver
	FreeSpace func(string) (uint64, error)
	OnEvent   func(model.Event)
	// Enrich runs after every file is locally verified but before the import is
	// marked local_complete. It must be best-effort: a failure is reported as a
	// warning, never as an import failure.
	Enrich func(ctx context.Context, im *model.Import) error
}

type Importer struct {
	store     *store.Store
	resolver  SourceResolver
	workers   int
	freeSpace func(string) (uint64, error)
	onEvent   func(model.Event)
	enrich    func(context.Context, *model.Import) error
	mu        sync.Mutex
}

func New(st *store.Store, opts Options) *Importer {
	imp := &Importer{
		store:     st,
		resolver:  opts.Resolver,
		workers:   opts.Workers,
		freeSpace: opts.FreeSpace,
		onEvent:   opts.OnEvent,
		enrich:    opts.Enrich,
	}
	if imp.resolver == nil {
		imp.resolver = dirResolver{}
	}
	if imp.freeSpace == nil {
		imp.freeSpace = fsx.FreeSpace
	}
	if imp.workers <= 0 {
		imp.workers = 2
	}
	if imp.workers > 8 {
		imp.workers = 8
	}
	return imp
}

// StartRequest carries everything the operator chose on the import screen.
type StartRequest struct {
	ProjectID     string
	ProjectCode   string
	ShootDate     string // YYYY-MM-DD
	Source        source.Source
	DestA         string
	DestB         string
	RequireBackup bool
	Card          model.Card
	// AllowDuplicate starts a second import for a card that already has an
	// active one, instead of returning ErrImportExists.
	AllowDuplicate bool
}

// Start validates destinations, records the card and discovers the card
// content. It does not copy anything; Run does.
func (imp *Importer) Start(ctx context.Context, req StartRequest) (*model.Import, int, error) {
	counts := 0
	projectCode := strings.TrimSpace(req.ProjectCode)
	if projectCode == "" {
		return nil, counts, errors.New("project code is required")
	}
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(req.ShootDate)); err != nil {
		return nil, counts, fmt.Errorf("shoot date must be YYYY-MM-DD: %w", err)
	}
	if req.Source == nil {
		return nil, counts, errors.New("source is required")
	}
	if err := ValidateDestinations(req.DestA, req.DestB, req.Source.Root(), req.RequireBackup); err != nil {
		return nil, counts, err
	}
	if err := ProbeWritable(req.DestA); err != nil {
		return nil, counts, fmt.Errorf("destination A is not writable: %w", err)
	}
	if req.RequireBackup {
		if err := ProbeWritable(req.DestB); err != nil {
			return nil, counts, fmt.Errorf("destination B is not writable: %w", err)
		}
	}

	card := req.Card
	if card.Fingerprint == "" {
		card = ManualCard(req.Source.Root())
	}
	if card.ID == "" {
		card.ID = uuid.NewString()
	}
	if card.Label == "" {
		card.Label = filepath.Base(req.Source.Root())
	}
	now := time.Now().UTC()
	if card.FirstSeen.IsZero() {
		card.FirstSeen = now
	}
	card.LastSeen = now
	stored, err := imp.store.UpsertCard(ctx, card)
	if err != nil {
		return nil, counts, err
	}

	if !req.AllowDuplicate {
		existing, err := imp.store.ActiveImportForCard(ctx, stored.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, counts, err
		}
		if existing != nil {
			return existing, 0, ErrImportExists
		}
	}

	entries, err := scan.Discover(ctx, req.Source.FS())
	if err != nil {
		return nil, counts, fmt.Errorf("scan source: %w", err)
	}
	if len(entries) == 0 {
		return nil, counts, ErrEmptySource
	}

	im := &model.Import{
		ID:            uuid.NewString(),
		ProjectID:     strings.TrimSpace(req.ProjectID),
		ProjectCode:   projectCode,
		CardID:        stored.ID,
		SourceRoot:    req.Source.Root(),
		ShootDate:     strings.TrimSpace(req.ShootDate),
		DestA:         req.DestA,
		DestB:         req.DestB,
		RequireBackup: req.RequireBackup,
		Status:        model.ImportOpen,
		CreatedAt:     now,
	}
	if !req.RequireBackup {
		im.DestB = ""
	}
	if err := imp.store.CreateImport(ctx, *im); err != nil {
		return nil, counts, err
	}

	files := make([]model.File, 0, len(entries))
	for _, e := range entries {
		files = append(files, model.File{
			ID:       uuid.NewString(),
			ImportID: im.ID,
			RelPath:  e.RelPath,
			Size:     e.Size,
			MtimeNS:  e.MtimeNS,
			Kind:     e.Kind,
			Role:     e.Role,
			PairKey:  e.PairKey,
			State:    model.StateDiscovered,
		})
	}
	added, err := imp.store.InsertFiles(ctx, files)
	if err != nil {
		return nil, counts, err
	}
	imp.emit(ctx, model.Event{
		Level: "info", Code: "import.created", ImportID: im.ID,
		Message: fmt.Sprintf("导入已创建：%s / %s / %d 个文件", projectCode, im.ShootDate, added),
	})
	if !req.RequireBackup {
		// The operator explicitly downgraded to one destination; keep the
		// warning visible until they acknowledge it.
		imp.emit(ctx, model.Event{
			Level: "warning", Code: "backup_disabled", ImportID: im.ID,
			Message: "备份目的地 B 已关闭：本导入只有一份本地副本，请确认这是有意为之。",
		})
	}
	return im, added, nil
}

// ManualCard derives a stable identity for a directory-based import (manual
// card reader, fixed disk). Real volumes use serial+capacity+label instead.
func ManualCard(root string) model.Card {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(abs))))
	return model.Card{
		Fingerprint: "dir:" + hex.EncodeToString(sum[:16]),
		Label:       filepath.Base(abs),
	}
}

// Result is the outcome of one copy pass.
type Result struct {
	ImportID      string `json:"import_id"`
	Paused        bool   `json:"paused"`
	Reason        string `json:"reason,omitempty"`
	Message       string `json:"message,omitempty"`
	Total         int    `json:"total"`
	LocalVerified int    `json:"local_verified"`
	Failed        int    `json:"failed"`
}

// Run copies everything not yet local. It is safe to call again after a pause:
// verified files are skipped and interrupted temps are recovered first.
func (imp *Importer) Run(ctx context.Context, importID string) (Result, error) {
	imp.mu.Lock()
	defer imp.mu.Unlock()

	res := Result{ImportID: importID}
	im, err := imp.store.Import(ctx, importID)
	if err != nil {
		return res, err
	}
	if im.Status == model.ImportComplete || im.Status == model.ImportLocalComplete {
		return imp.result(ctx, im, res)
	}

	card, err := imp.store.Card(ctx, im.CardID)
	if err != nil {
		return res, fmt.Errorf("load card: %w", err)
	}

	temps, err := imp.store.ResetInterrupted(ctx)
	if err != nil {
		return res, err
	}
	for _, p := range temps {
		_ = fsx.RemoveIfExists(p)
	}
	if err := imp.store.SetPauseReason(ctx, im.ID, ""); err != nil {
		return res, err
	}

	src, err := imp.resolver.Resolve(ctx, im)
	if err != nil {
		return imp.pause(ctx, im, fsx.ReasonCardRemoved, "存储卡不可用，导入已暂停；重新插入后继续。")
	}

	dests := destinations(im)
	if err := ValidateDestinations(im.DestA, im.DestB, src.Root(), im.RequireBackup); err != nil {
		return imp.pause(ctx, im, fsx.ReasonDestination, "目标目录不可用："+err.Error())
	}
	for _, d := range dests {
		if err := ProbeWritable(d.Root); err != nil {
			return imp.pause(ctx, im, fsx.ReasonDestination,
				fmt.Sprintf("目标 %s 不可写：%v", d.Name, err))
		}
	}

	files, err := imp.store.Files(ctx, im.ID)
	if err != nil {
		return res, err
	}
	if msg, ok := imp.checkSpace(dests, files); !ok {
		return imp.pause(ctx, im, fsx.ReasonDiskFull, msg)
	}

	var queue []model.File
	for _, f := range files {
		if !f.State.LocallyDone() {
			queue = append(queue, f)
		}
	}
	if len(queue) == 0 {
		if err := imp.runEnrich(ctx, im); err != nil {
			return res, err
		}
		return imp.completeLocal(ctx, im, res)
	}

	layout := copy.Layout{ProjectCode: im.ProjectCode, ShootDate: im.ShootDate, CardLabel: card.Label}
	engine := &copy.Engine{Store: imp.store, Dests: dests}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		stopReason fsx.Reason
		stopMsg    string
	)
	jobs := make(chan model.File)
	for i := 0; i < imp.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if runCtx.Err() != nil {
					continue
				}
				err := engine.RunFile(runCtx, src, f, layout)
				if err == nil {
					continue
				}
				var stop *copy.StopError
				if errors.As(err, &stop) {
					mu.Lock()
					if stopReason == "" {
						stopReason, stopMsg = stop.Reason, stop.Error()
					}
					mu.Unlock()
					cancel()
					continue
				}
				if runCtx.Err() != nil {
					continue
				}
				imp.emit(ctx, model.Event{
					Level: "warning", Code: "file_copy_failed", ImportID: im.ID, FileID: f.ID,
					Message: fmt.Sprintf("%s：%v", f.RelPath, err),
				})
			}
		}()
	}

feed:
	for _, f := range queue {
		select {
		case jobs <- f:
		case <-runCtx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	if stopReason != "" {
		return imp.pause(ctx, im, stopReason, stopMessage(stopReason, stopMsg))
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	counts, err := imp.store.FileCounts(ctx, im.ID)
	if err != nil {
		return res, err
	}
	if counts.Total > 0 && counts.LocalVerified == counts.Total {
		if err := imp.runEnrich(ctx, im); err != nil {
			return res, err
		}
		return imp.completeLocal(ctx, im, res)
	}
	imp.emit(ctx, model.Event{
		Level: "warning", Code: "import.copy_incomplete", ImportID: im.ID,
		Message: fmt.Sprintf("本轮拷贝完成：%d/%d 个文件已本地校验，%d 个失败，可重试。",
			counts.LocalVerified, counts.Total, counts.Failed),
	})
	return fillCounts(res, counts), nil
}

// Status returns the persisted view the UI polls.
type Status struct {
	Import model.Import     `json:"import"`
	Counts store.FileCounts `json:"counts"`
	Events []model.Event    `json:"unacknowledged_events"`
}

func (imp *Importer) Status(ctx context.Context, importID string) (Status, error) {
	im, err := imp.store.Import(ctx, importID)
	if err != nil {
		return Status{}, err
	}
	counts, err := imp.store.FileCounts(ctx, importID)
	if err != nil {
		return Status{}, err
	}
	events, err := imp.store.UnacknowledgedEvents(ctx, 50)
	if err != nil {
		return Status{}, err
	}
	mine := make([]model.Event, 0, len(events))
	for _, e := range events {
		if e.ImportID == "" || e.ImportID == importID {
			mine = append(mine, e)
		}
	}
	return Status{Import: *im, Counts: counts, Events: mine}, nil
}

func (imp *Importer) checkSpace(dests []copy.Dest, files []model.File) (string, bool) {
	var remaining int64
	for _, f := range files {
		if !f.State.LocallyDone() {
			remaining += f.Size
		}
	}
	if remaining == 0 {
		return "", true
	}
	need := uint64(remaining) + uint64(remaining/10) + minHeadroomBytes
	for _, d := range dests {
		free, err := imp.freeSpace(d.Root)
		if err != nil {
			return fmt.Sprintf("无法读取目标 %s 的剩余空间：%v", d.Name, err), false
		}
		if free < need {
			return fmt.Sprintf("目标 %s 剩余空间不足：本批需要约 %s（含 10%% 余量 + 1 GiB），当前可用 %s。",
				d.Name, humanBytes(need), humanBytes(free)), false
		}
	}
	return "", true
}

func (imp *Importer) completeLocal(ctx context.Context, im *model.Import, res Result) (Result, error) {
	ok, err := imp.store.SetImportStatus(ctx, im.ID, []model.ImportStatus{model.ImportOpen}, model.ImportLocalComplete)
	if err != nil {
		return res, err
	}
	if ok {
		imp.emit(ctx, model.Event{
			Level: "info", Code: "import.local_complete", ImportID: im.ID,
			Message: "本地拷贝完成：所有文件的两份副本均已与源文件 SHA-256 一致。",
		})
	}
	im.Status = model.ImportLocalComplete
	return imp.result(ctx, im, res)
}

func (imp *Importer) pause(ctx context.Context, im *model.Import, reason fsx.Reason, msg string) (Result, error) {
	if err := imp.store.SetPauseReason(ctx, im.ID, string(reason)); err != nil {
		return Result{ImportID: im.ID}, err
	}
	imp.emit(ctx, model.Event{
		Level: "error", Code: "import_paused", ImportID: im.ID,
		Message: fmt.Sprintf("导入已暂停（%s）：%s", reason, msg),
	})
	counts, err := imp.store.FileCounts(ctx, im.ID)
	if err != nil {
		return Result{ImportID: im.ID}, err
	}
	res := Result{
		ImportID: im.ID, Paused: true, Reason: string(reason), Message: msg,
	}
	return fillCounts(res, counts), nil
}

func (imp *Importer) result(ctx context.Context, im *model.Import, res Result) (Result, error) {
	counts, err := imp.store.FileCounts(ctx, im.ID)
	if err != nil {
		return res, err
	}
	return fillCounts(res, counts), nil
}

func (imp *Importer) emit(ctx context.Context, e model.Event) {
	_, _ = imp.store.AddEvent(ctx, e)
	if imp.onEvent != nil {
		imp.onEvent(e)
	}
}

// runEnrich derives metadata after the local copies are verified. A canceled
// context propagates (the operator paused); other problems only warn, because
// the two verified copies are already the acceptance criterion.
func (imp *Importer) runEnrich(ctx context.Context, im *model.Import) error {
	if imp.enrich == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := imp.enrich(ctx, im); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		imp.emit(ctx, model.Event{
			Level: "warning", Code: "metadata_failed", ImportID: im.ID,
			Message: "EXIF 元数据写入失败：" + err.Error(),
		})
	}
	return nil
}

func fillCounts(res Result, c store.FileCounts) Result {
	res.Total = c.Total
	res.LocalVerified = c.LocalVerified
	res.Failed = c.Failed
	return res
}

func stopMessage(reason fsx.Reason, detail string) string {
	switch reason {
	case fsx.ReasonCardRemoved:
		return "存储卡已拔出或不可用，导入已暂停；重新插入后继续。"
	case fsx.ReasonDiskFull:
		return "磁盘空间不足，导入已暂停；清理空间后继续，未完成的文件不会发布。"
	case fsx.ReasonDestination:
		return "目标目录不可写：" + detail
	}
	return detail
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
