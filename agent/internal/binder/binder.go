// Package binder is the thin Wails-facing adapter over the agent core. Every
// method here is callable from the Vue frontend; long-running work happens in a
// background goroutine and the UI polls Snapshot (events are pushed as well).
package binder

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"photography-workflow/agent/internal/api"
	"photography-workflow/agent/internal/card"
	"photography-workflow/agent/internal/core"
	"photography-workflow/agent/internal/derive"
	"photography-workflow/agent/internal/fsx"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/remote"
	"photography-workflow/agent/internal/selftest"
	"photography-workflow/agent/internal/settings"
	"photography-workflow/agent/internal/source"
	"photography-workflow/agent/internal/store"
	"photography-workflow/agent/internal/tool"
	"photography-workflow/agent/internal/upload"
)

// Version is the agent build version, shared with the API client so the
// User-Agent and device registrations agree. Release builds override it with
// -ldflags "-X .../api.Version=<version>".
var Version = api.Version

var ErrBusy = errors.New("已有导入正在运行，请等待完成或暂停后再开始新的导入")

// Emitter forwards UI events. Wails wires it to runtime.EventsEmit.
type Emitter func(name string, data any)

// App is the Wails-bound application object.
type App struct {
	dbPath       string
	settingsPath string
	emit         Emitter

	mu        sync.Mutex
	baseCtx   context.Context
	st        *store.Store
	deriver   *derive.Derive
	startErr  error
	closing   bool
	running   string
	cancel    context.CancelFunc
	userPause bool
	lastErr   string
	lastRes   *core.Result
	wg        sync.WaitGroup

	// Server connection (see server.go). remote holds the cached client for
	// the current settings; serverErr is the last actionable failure.
	remote    *remote.Conn
	remoteKey string
	serverErr string
	hbCancel  context.CancelFunc

	// Upload slot (see upload.go); independent of the copy slot because a
	// completed copy can upload while the operator starts the next import.
	uploading     string
	uploadCancel  context.CancelFunc
	uploadPaused  bool
	lastUpload    *upload.Result
	lastUploadErr string

	// Update slot (see update.go). upd is resolved during Startup; the check
	// loop is canceled by updCancel on shutdown.
	upd       *updateRuntime
	updCancel context.CancelFunc
	installer func(path string) error
}

func New(dbPath, settingsPath string, emit Emitter) *App {
	return &App{dbPath: dbPath, settingsPath: settingsPath, emit: emit}
}

// Startup opens the state database and cleans up what a previous crash left
// behind: interrupted copies go back to pending and orphan temp files vanish.
func (a *App) Startup(ctx context.Context) {
	a.mu.Lock()
	a.baseCtx = ctx
	a.mu.Unlock()

	st, err := store.Open(ctx, a.dbPath)
	if err != nil {
		a.mu.Lock()
		a.startErr = err
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	a.st = st
	a.mu.Unlock()

	prefs, _ := settings.Load(a.settingsPath)
	paths := tool.Locate(prefs.ExifToolPath, prefs.FFmpegPath)
	deriver := derive.New(st,
		filepath.Join(filepath.Dir(a.dbPath), "cache", "previews"),
		paths.ExifTool, paths.FFmpeg,
		func(e model.Event) { a.emitEvent("import:event", e) })
	a.mu.Lock()
	a.deriver = deriver
	a.mu.Unlock()

	temps, err := st.ResetInterrupted(ctx)
	if err != nil {
		a.addEvent(ctx, model.Event{
			Level: "warning", Code: "startup_recovery_failed",
			Message: "启动恢复未完成：" + err.Error(),
		})
		return
	}
	for _, p := range temps {
		_ = fsx.RemoveIfExists(p)
	}
	if len(temps) > 0 {
		a.addEvent(ctx, model.Event{
			Level: "info", Code: "startup_recovery",
			Message: fmt.Sprintf("已清理 %d 个上次中断留下的临时文件，相关文件将重新拷贝。", len(temps)),
		})
	}

	// Restore the server connection and restart the newest interrupted upload
	// so a crash between "local complete" and "remote verified" heals itself.
	a.prepareServer()
	a.resumeUploads()

	// A build that rolled back left a notice; surface it as an alert, then
	// start the signed update checker.
	a.consumeRollbackNotice(ctx)
	a.initUpdates()
	a.startUpdates(ctx)
}

// Shutdown stops a running import and closes the database.
func (a *App) Shutdown(ctx context.Context) {
	a.mu.Lock()
	a.closing = true
	cancel := a.cancel
	uploadCancel := a.uploadCancel
	updCancel := a.updCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if uploadCancel != nil {
		uploadCancel()
	}
	if updCancel != nil {
		updCancel()
	}
	a.stopHeartbeat()

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(15 * time.Second):
	}

	a.mu.Lock()
	st := a.st
	a.st = nil
	dv := a.deriver
	a.deriver = nil
	a.mu.Unlock()
	if dv != nil {
		_ = dv.Close()
	}
	if st != nil {
		_ = st.Close()
	}
}

// ---------- health and settings ----------

type Health struct {
	Version      string `json:"version"`
	DBPath       string `json:"db_path"`
	SettingsPath string `json:"settings_path"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
}

func (a *App) Health() Health {
	a.mu.Lock()
	err := a.startErr
	ok := a.st != nil
	a.mu.Unlock()
	h := Health{
		Version: Version, DBPath: a.dbPath, SettingsPath: a.settingsPath, OK: ok,
	}
	if err != nil {
		h.Error = err.Error()
	}
	return h
}

func (a *App) GetSettings() (settings.Settings, error) {
	return settings.Load(a.settingsPath)
}

func (a *App) SaveSettings(s settings.Settings) error {
	if err := settings.Save(a.settingsPath, s); err != nil {
		return err
	}
	paths := tool.Locate(s.ExifToolPath, s.FFmpegPath)
	a.mu.Lock()
	dv := a.deriver
	a.mu.Unlock()
	if dv != nil {
		dv.SetTools(paths.ExifTool, paths.FFmpeg)
	}
	// Server URL, tenant and bandwidth may have changed: rebuild the client
	// and re-arm heartbeats. Running uploads keep their existing client.
	a.invalidateConnection()
	a.stopHeartbeat()
	a.prepareServer()
	return nil
}

func (a *App) DefaultSettings() settings.Settings {
	return settings.Default()
}

// ---------- cards ----------

type CardInfo struct {
	Root        string `json:"root"`
	GUID        string `json:"guid"`
	Label       string `json:"label"`
	Serial      uint32 `json:"serial"`
	Capacity    uint64 `json:"capacity"`
	Fingerprint string `json:"fingerprint"`
}

func (a *App) Cards() ([]CardInfo, error) {
	vols, err := card.Detect(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]CardInfo, 0, len(vols))
	for _, v := range vols {
		out = append(out, CardInfo{
			Root: v.Root, GUID: v.GUID, Label: v.Label, Serial: v.Serial,
			Capacity: v.Capacity, Fingerprint: v.Fingerprint(),
		})
	}
	return out, nil
}

type DestinationInfo struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Writable  bool   `json:"writable"`
	FreeBytes uint64 `json:"free_bytes"`
	Error     string `json:"error,omitempty"`
}

// CheckDestination lets the settings screen validate a path before an import.
func (a *App) CheckDestination(path string) DestinationInfo {
	path = strings.TrimSpace(path)
	info := DestinationInfo{Path: path}
	if path == "" {
		info.Error = "路径为空"
		return info
	}
	exists, err := fsx.Exists(path)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Exists = exists
	if !exists {
		info.Error = "目录不存在或不可访问"
		return info
	}
	if err := core.ProbeWritable(path); err != nil {
		info.Error = err.Error()
		return info
	}
	info.Writable = true
	if free, err := fsx.FreeSpace(path); err == nil {
		info.FreeBytes = free
	}
	return info
}

// ---------- imports ----------

type StartRequest struct {
	Source         string `json:"source"`
	ProjectCode    string `json:"project_code"`
	ProjectID      string `json:"project_id"`
	ShootDate      string `json:"shoot_date"`
	DestA          string `json:"dest_a"`
	DestB          string `json:"dest_b"`
	RequireBackup  bool   `json:"require_backup"`
	AllowDuplicate bool   `json:"allow_duplicate"`
}

type StartResult struct {
	Import   *model.Import `json:"import"`
	Added    int           `json:"added"`
	Existing bool          `json:"existing"`
}

// StartImport records the import and returns immediately; copying continues in
// the background (watch Snapshot and the import:event stream).
func (a *App) StartImport(req StartRequest) (*StartResult, error) {
	st, err := a.ready()
	if err != nil {
		return nil, err
	}
	req.Source = strings.TrimSpace(req.Source)
	req.ProjectCode = strings.TrimSpace(req.ProjectCode)
	req.ShootDate = strings.TrimSpace(req.ShootDate)
	req.DestA = strings.TrimSpace(req.DestA)
	req.DestB = strings.TrimSpace(req.DestB)
	if req.Source == "" {
		return nil, errors.New("请选择存储卡或要导入的目录")
	}
	if req.ProjectCode == "" {
		return nil, errors.New("请填写项目编号")
	}
	if req.ShootDate == "" {
		req.ShootDate = time.Now().Format("2006-01-02")
	}

	s, err := settings.Load(a.settingsPath)
	if err != nil {
		return nil, err
	}
	if req.DestA == "" {
		req.DestA = s.DestA
	}
	if req.DestB == "" {
		req.DestB = s.DestB
	}
	if req.DestA == "" {
		return nil, errors.New("请先设置目的地 A 目录")
	}
	if req.RequireBackup && req.DestB == "" {
		return nil, errors.New("已启用备份，请设置目的地 B；如需单目的地，请显式关闭备份")
	}

	if a.isRunning() {
		return nil, ErrBusy
	}

	src, err := source.NewDir(req.Source)
	if err != nil {
		return nil, err
	}
	importer := a.newImporter(st)
	im, added, err := importer.Start(context.Background(), core.StartRequest{
		ProjectID:      strings.TrimSpace(req.ProjectID),
		ProjectCode:    req.ProjectCode,
		ShootDate:      req.ShootDate,
		Source:         src,
		DestA:          req.DestA,
		DestB:          req.DestB,
		RequireBackup:  req.RequireBackup,
		Card:           card.ForRoot(context.Background(), src.Root()),
		AllowDuplicate: req.AllowDuplicate,
	})
	if errors.Is(err, core.ErrImportExists) {
		return &StartResult{Import: im, Existing: true}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := a.launch(im.ID); err != nil {
		return nil, err
	}
	return &StartResult{Import: im, Added: added}, nil
}

// RunImport resumes a paused or partially failed import.
func (a *App) RunImport(id string) error {
	_, err := a.ready()
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("缺少导入 ID")
	}
	if a.isRunning() {
		return ErrBusy
	}
	a.mu.Lock()
	st := a.st
	a.mu.Unlock()
	im, err := st.Import(context.Background(), id)
	if err != nil {
		return err
	}
	if im.Status == model.ImportLocalComplete || im.Status == model.ImportComplete {
		return errors.New("该导入的本地拷贝已完成，无需继续")
	}
	return a.launch(id)
}

// PauseImport cancels the running copy pass. Every file already published has
// been verified; unfinished temp files are cleaned when the run returns.
func (a *App) PauseImport() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		return errors.New("当前没有正在运行的导入")
	}
	a.userPause = true
	a.cancel()
	return nil
}

func (a *App) isRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running != ""
}

func (a *App) launch(id string) error {
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return errors.New("应用正在退出")
	}
	if a.running != "" {
		a.mu.Unlock()
		return ErrBusy
	}
	st := a.st
	base := a.baseCtx
	if base == nil {
		base = context.Background()
	}
	runCtx, cancel := context.WithCancel(base)
	a.running = id
	a.cancel = cancel
	a.userPause = false
	a.lastErr = ""
	a.lastRes = nil
	a.wg.Add(1)
	a.mu.Unlock()

	if _, err := settings.Load(a.settingsPath); err != nil {
		cancel()
		a.mu.Lock()
		a.running, a.cancel = "", nil
		a.mu.Unlock()
		a.wg.Done()
		return err
	}

	importer := a.newImporter(st)
	go a.run(runCtx, cancel, importer, id)
	return nil
}

func (a *App) run(ctx context.Context, cancel context.CancelFunc, importer *core.Importer, id string) {
	defer a.wg.Done()
	defer cancel()

	res, err := importer.Run(ctx, id)

	a.mu.Lock()
	userPause := a.userPause
	a.running = ""
	a.cancel = nil
	a.userPause = false
	if err != nil && !errors.Is(err, context.Canceled) {
		a.lastErr = err.Error()
	}
	if err == nil {
		r := res
		a.lastRes = &r
	}
	a.mu.Unlock()

	if userPause {
		// The run context is canceled, so use a fresh context to persist the
		// operator's intent; otherwise a restart would resume unexpectedly.
		bg := context.Background()
		if serr := a.st.SetPauseReason(bg, id, "user"); serr == nil {
			a.addEvent(bg, model.Event{
				Level: "info", Code: "import.paused_by_user", ImportID: id,
				Message: "导入已由用户暂停；点击“继续”即可从断点恢复。",
			})
		}
	}

	a.emitEvent("import:finished", map[string]any{
		"import_id":      id,
		"result":         res,
		"error":          errString(err),
		"paused_by_user": userPause,
	})
	a.maybeStartUpload(id, err)
}

// maybeStartUpload hands a freshly completed local copy to the upload slot.
// A missing project or session is not an error here: the operator can start
// the upload later from the dashboard.
func (a *App) maybeStartUpload(id string, runErr error) {
	if runErr != nil {
		return
	}
	st, err := a.ready()
	if err != nil {
		return
	}
	im, err := st.Import(context.Background(), id)
	if err != nil || im.Status != model.ImportLocalComplete || im.ProjectID == "" {
		return
	}
	if err := a.startUpload(id); err != nil {
		switch {
		case errors.Is(err, ErrLoginRequired), errors.Is(err, remote.ErrNoServer),
			errors.Is(err, ErrProjectRequired), errors.Is(err, ErrUploadBusy):
			return
		}
		a.addEvent(context.Background(), modelEvent("warning", "upload_start_failed",
			"自动上传未能开始："+err.Error()))
	}
}

func (a *App) newImporter(st *store.Store) *core.Importer {
	s, _ := settings.Load(a.settingsPath)
	a.mu.Lock()
	dv := a.deriver
	a.mu.Unlock()
	opts := core.Options{
		Workers:  s.Workers,
		Resolver: card.Resolver{Store: st},
		OnEvent: func(e model.Event) {
			a.emitEvent("import:event", e)
		},
	}
	if dv != nil {
		opts.Enrich = dv.Enrich
	}
	return core.New(st, opts)
}

// PreviewFile returns the cached 512px JPEG for one imported file as a data
// URL, generating it on first request. Wails cannot serve local files to the
// webview, so the bytes travel inline.
func (a *App) PreviewFile(fileID string) (PreviewResult, error) {
	st, err := a.ready()
	if err != nil {
		return PreviewResult{}, err
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return PreviewResult{}, errors.New("缺少文件 ID")
	}
	a.mu.Lock()
	dv := a.deriver
	a.mu.Unlock()
	if dv == nil {
		return PreviewResult{}, errors.New("预览服务尚未就绪")
	}
	ctx := context.Background()
	f, err := st.File(ctx, fileID)
	if err != nil {
		return PreviewResult{}, err
	}
	path, err := dv.Preview(ctx, *f)
	if err != nil {
		return PreviewResult{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return PreviewResult{}, err
	}
	if len(data) > maxPreviewBytes {
		return PreviewResult{}, fmt.Errorf("预览文件异常（%d 字节），已拒绝加载", len(data))
	}
	return PreviewResult{
		FileID:  fileID,
		DataURL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data),
		Size:    len(data),
	}, nil
}

type PreviewResult struct {
	FileID  string `json:"file_id"`
	DataURL string `json:"data_url"`
	Size    int    `json:"size"`
}

// maxPreviewBytes bounds the inline payload sent to the webview. A 512px JPEG
// is tens of KiB; anything near this limit means a broken cache entry.
const maxPreviewBytes = 4 << 20

// ---------- status for the UI ----------

type Snapshot struct {
	Running        bool             `json:"running"`
	Import         *model.Import    `json:"import,omitempty"`
	Counts         store.FileCounts `json:"counts"`
	Alerts         []model.Event    `json:"alerts"`
	UnackedTotal   int              `json:"unacked_total"`
	LastResult     *core.Result     `json:"last_result,omitempty"`
	LastError      string           `json:"last_error,omitempty"`
	BackupDisabled bool             `json:"backup_disabled"`
	Server         ServerState      `json:"server"`
	Upload         UploadState      `json:"upload"`
	Update         UpdateState      `json:"update"`
}

func (a *App) Snapshot() (Snapshot, error) {
	st, err := a.ready()
	if err != nil {
		return Snapshot{}, err
	}
	ctx := context.Background()
	var snap Snapshot

	a.mu.Lock()
	runID := a.running
	snap.LastResult = a.lastRes
	snap.LastError = a.lastErr
	a.mu.Unlock()
	snap.Running = runID != ""

	var im *model.Import
	if runID != "" {
		im, err = st.Import(ctx, runID)
		if err != nil {
			return snap, err
		}
	} else {
		recent, err := st.Imports(ctx, 1)
		if err != nil {
			return snap, err
		}
		if len(recent) > 0 {
			im = &recent[0]
		}
	}
	if im != nil {
		snap.Import = im
		if snap.Counts, err = st.FileCounts(ctx, im.ID); err != nil {
			return snap, err
		}
	}

	all, err := st.UnacknowledgedEvents(ctx, 200)
	if err != nil {
		return snap, err
	}
	snap.UnackedTotal = len(all)
	snap.Alerts = make([]model.Event, 0, len(all))
	for _, e := range all {
		if im == nil || e.ImportID == "" || e.ImportID == im.ID {
			snap.Alerts = append(snap.Alerts, e)
		}
	}
	if s, serr := settings.Load(a.settingsPath); serr == nil {
		snap.BackupDisabled = !s.RequireBackup
	}
	if im != nil && !im.RequireBackup {
		snap.BackupDisabled = true
	}
	snap.Server = a.Server()
	snap.Upload = a.uploadState()
	snap.Update = a.UpdateStatus()
	return snap, nil
}

type FileRow struct {
	File    model.File        `json:"file"`
	Copies  []model.Copy      `json:"copies"`
	Preview *model.Derivative `json:"preview,omitempty"`
}

type ImportDetail struct {
	Status core.Status `json:"status"`
	Files  []FileRow   `json:"files"`
	Total  int         `json:"total_files"`
}

// ImportDetail returns the persisted status plus up to limit file rows (each
// with its per-destination copy state) for the history screen.
func (a *App) ImportDetail(id string, limit int) (*ImportDetail, error) {
	st, err := a.ready()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	importer := a.newImporter(st)
	status, err := importer.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	files, err := st.Files(ctx, id)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	detail := &ImportDetail{Status: status, Total: len(files), Files: make([]FileRow, 0, len(files))}
	previews := map[string]*model.Derivative{}
	if rows, derr := st.Derivatives(ctx, id); derr == nil {
		for i := range rows {
			if rows[i].Kind == model.DerivativeKindGallery {
				previews[rows[i].FileID] = &rows[i]
			}
		}
	}
	for i, f := range files {
		if i >= limit {
			break
		}
		copies, err := st.Copies(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		if copies == nil {
			copies = []model.Copy{}
		}
		row := FileRow{File: f, Copies: copies}
		row.Preview = previews[f.ID]
		detail.Files = append(detail.Files, row)
	}
	return detail, nil
}

type ImportRow struct {
	Import    model.Import     `json:"import"`
	Counts    store.FileCounts `json:"counts"`
	CardLabel string           `json:"card_label"`
}

func (a *App) Imports(limit int) ([]ImportRow, error) {
	st, err := a.ready()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	imports, err := st.Imports(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ImportRow, 0, len(imports))
	for _, im := range imports {
		counts, err := st.FileCounts(ctx, im.ID)
		if err != nil {
			return nil, err
		}
		label := ""
		if c, cerr := st.Card(ctx, im.CardID); cerr == nil {
			label = c.Label
		}
		out = append(out, ImportRow{Import: im, Counts: counts, CardLabel: label})
	}
	return out, nil
}

// ---------- alerts and history ----------

func (a *App) Alerts() ([]model.Event, error) {
	st, err := a.ready()
	if err != nil {
		return nil, err
	}
	return nonNilEvents(st.UnacknowledgedEvents(context.Background(), 200))
}

func (a *App) AckAlert(id int64) (bool, error) {
	st, err := a.ready()
	if err != nil {
		return false, err
	}
	return st.AckEvent(context.Background(), id)
}

func (a *App) AckAllAlerts() (int, error) {
	st, err := a.ready()
	if err != nil {
		return 0, err
	}
	ctx := context.Background()
	events, err := st.UnacknowledgedEvents(ctx, 500)
	if err != nil {
		return 0, err
	}
	acked := 0
	for _, e := range events {
		ok, err := st.AckEvent(ctx, e.ID)
		if err != nil {
			return acked, err
		}
		if ok {
			acked++
		}
	}
	return acked, nil
}

func (a *App) RecentEvents(limit int) ([]model.Event, error) {
	st, err := a.ready()
	if err != nil {
		return nil, err
	}
	return nonNilEvents(st.RecentEvents(context.Background(), limit))
}

// nonNilEvents keeps the wire format stable: an empty list is [], never null.
func nonNilEvents(events []model.Event, err error) ([]model.Event, error) {
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = []model.Event{}
	}
	return events, nil
}

// ---------- maintenance ----------

func (a *App) Selftest() selftest.Report {
	return selftest.Run(context.Background())
}

func (a *App) VersionString() string { return Version }

// ---------- helpers ----------

func (a *App) ready() (*store.Store, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.startErr != nil {
		return nil, a.startErr
	}
	if a.st == nil {
		return nil, errors.New("本地数据库尚未打开")
	}
	return a.st, nil
}

func (a *App) addEvent(ctx context.Context, e model.Event) {
	a.mu.Lock()
	st := a.st
	a.mu.Unlock()
	if st == nil {
		return
	}
	_, _ = st.AddEvent(ctx, e)
}

func (a *App) emitEvent(name string, data any) {
	if a.emit != nil {
		a.emit(name, data)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
