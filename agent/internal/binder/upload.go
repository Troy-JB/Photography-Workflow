package binder

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"photography-workflow/agent/internal/derive"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/remote"
	"photography-workflow/agent/internal/settings"
	"photography-workflow/agent/internal/upload"
)

// ErrUploadBusy means another import is already uploading.
var ErrUploadBusy = errors.New("已有上传任务正在运行，请等待完成或暂停")

// UploadState is what the dashboard renders for the upload phase.
type UploadState struct {
	ImportID string         `json:"import_id,omitempty"`
	Running  bool           `json:"running"`
	Result   *upload.Result `json:"result,omitempty"`
	Error    string         `json:"error,omitempty"`
}

func (a *App) uploadState() UploadState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return UploadState{
		ImportID: a.uploading,
		Running:  a.uploading != "",
		Result:   a.lastUpload,
		Error:    a.lastUploadErr,
	}
}

// StartUpload resumes or begins the upload of a locally complete import. The
// copy must be finished: the orchestrator reads verified copies, never the
// card.
func (a *App) StartUpload(importID string) error {
	return a.startUpload(importID)
}

// PauseUpload cancels the running upload; finished parts keep their ETags and
// the next pass resumes from where it stopped.
func (a *App) PauseUpload() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.uploadCancel == nil {
		return errors.New("当前没有正在运行的上传")
	}
	a.uploadPaused = true
	a.uploadCancel()
	return nil
}

func (a *App) startUpload(id string) error {
	st, err := a.ready()
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("缺少导入 ID")
	}
	conn, err := a.connection()
	if err != nil {
		return err
	}
	if !conn.LoggedIn() {
		return ErrLoginRequired
	}
	ctx := context.Background()
	im, err := st.Import(ctx, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(im.ProjectID) == "" {
		return ErrProjectRequired
	}
	if im.Status != model.ImportLocalComplete && im.Status != model.ImportUploading {
		return fmt.Errorf("导入状态为 %s，只有本地校验完成的导入才能上传", im.Status)
	}

	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return errors.New("应用正在退出")
	}
	if a.uploading != "" {
		a.mu.Unlock()
		return ErrUploadBusy
	}
	base := a.baseCtx
	if base == nil {
		base = context.Background()
	}
	runCtx, cancel := context.WithCancel(base)
	dv := a.deriver
	a.uploading = id
	a.uploadCancel = cancel
	a.uploadPaused = false
	a.lastUpload = nil
	a.lastUploadErr = ""
	a.wg.Add(1)
	a.mu.Unlock()

	workers := 2
	if s, serr := settings.Load(a.settingsPath); serr == nil && s.Workers > 0 {
		workers = s.Workers
	}
	go a.runUpload(runCtx, cancel, conn, id, workers, dv)
	return nil
}

func (a *App) runUpload(ctx context.Context, cancel context.CancelFunc, conn *remote.Conn, id string, workers int, dv *derive.Derive) {
	defer a.wg.Done()
	defer cancel()

	st, _ := a.ready()
	res := upload.Result{ImportID: id}
	var err error
	if st != nil {
		var deviceID string
		deviceID, err = conn.EnsureDevice(ctx)
		if err == nil {
			opts := upload.Options{
				DeviceID: deviceID,
				Workers:  workers,
				OnEvent: func(e model.Event) {
					a.emitEvent("upload:event", e)
				},
			}
			if dv != nil {
				opts.Previews = dv
			}
			up := upload.New(st, conn.API(), opts)
			res, err = up.Run(ctx, id)
		}
	}

	a.mu.Lock()
	userPause := a.uploadPaused
	a.uploading = ""
	a.uploadCancel = nil
	a.uploadPaused = false
	if err != nil && !errors.Is(err, context.Canceled) {
		a.lastUploadErr = err.Error()
	}
	if err == nil {
		r := res
		a.lastUpload = &r
	}
	a.mu.Unlock()

	if userPause && st != nil {
		// The run context is canceled; persist the operator's intent so a
		// restart does not resume on its own.
		bg := context.Background()
		if serr := st.SetPauseReason(bg, id, "user"); serr == nil {
			a.addEvent(bg, modelEvent("info", "upload.paused_by_user",
				"上传已由用户暂停；点击“继续上传”即可从断点恢复。"))
		}
	}

	a.emitEvent("upload:finished", map[string]any{
		"import_id":      id,
		"result":         res,
		"error":          errString(err),
		"paused_by_user": userPause,
	})
}

// resumeUploads restarts the most recent interrupted upload after a crash.
// Only the newest import is resumed; older ones stay paused until an operator
// chooses them, so a restart never floods the server with parallel batches.
func (a *App) resumeUploads() {
	st, err := a.ready()
	if err != nil {
		return
	}
	conn, err := a.connection()
	if err != nil || !conn.LoggedIn() {
		return
	}
	imports, err := st.Imports(context.Background(), 20)
	if err != nil {
		return
	}
	for i := range imports {
		im := imports[i]
		if im.ProjectID == "" {
			continue
		}
		if im.Status != model.ImportUploading && im.Status != model.ImportLocalComplete {
			continue
		}
		// Resume only if there is remote work left; a completed import is
		// filtered by its status above.
		if err := a.startUpload(im.ID); err != nil {
			if !errors.Is(err, ErrUploadBusy) {
				a.setServerError(err.Error())
			}
		}
		return
	}
}
