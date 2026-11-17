package binder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/updater"
)

// updateCheckInterval is the background cadence. Startup performs an extra
// check so a workstation that stays powered for weeks still sees releases.
const updateCheckInterval = 6 * time.Hour

// updateCheckTimeout bounds a manifest fetch; updateDownloadTimeout bounds a
// slow installer download without cancelling a healthy large transfer.
const (
	updateCheckTimeout    = 45 * time.Second
	updateDownloadTimeout = 30 * time.Minute
)

// UpdateState is what the settings screen renders for the update panel.
type UpdateState struct {
	// Configured is false when this build carries no update source.
	Configured bool `json:"configured"`
	// Supported is false for development layouts (build/bin), where an update
	// can be checked and downloaded but not installed.
	Supported      bool   `json:"supported"`
	Channel        string `json:"channel,omitempty"`
	Current        string `json:"current"`
	Latest         string `json:"latest,omitempty"`
	Available      bool   `json:"available"`
	Mandatory      bool   `json:"mandatory"`
	Notes          string `json:"notes,omitempty"`
	Checking       bool   `json:"checking"`
	Downloaded     bool   `json:"downloaded"`
	DownloadedSize int64  `json:"downloaded_size,omitempty"`
	Downloading    bool   `json:"downloading"`
	Progress       int    `json:"progress"`
	Installing     bool   `json:"installing"`
	// Unsigned warns that a dev-channel installer carries no Authenticode
	// signature; stable builds refuse such packages outright.
	Unsigned  bool   `json:"unsigned"`
	CheckedAt string `json:"checked_at,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
}

type updateRuntime struct {
	cfg       updater.Config
	cfgErr    error
	root      string
	version   string
	supported bool

	manifest    updater.Manifest
	available   bool
	mandatory   bool
	checking    bool
	checkedAt   time.Time
	downloading bool
	downloaded  string // local installer path; never exposed to the frontend
	downBytes   int64
	progress    int
	installing  bool
	err         string
	message     string
}

// initUpdates resolves the release identity once. A build without a manifest
// URL or pinned key simply reports Configured=false; it is not an error.
func (a *App) initUpdates() {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	root, version, supported := updater.InstallRoot(exe)
	cfg, cfgErr := updater.ResolveConfig(filepath.Join(filepath.Dir(a.dbPath), "updates"))

	u := &updateRuntime{cfg: cfg, cfgErr: cfgErr, root: root, supported: supported}
	if !supported || version == "" {
		u.version = Version
	} else {
		u.version = version
	}
	if cfgErr != nil {
		u.err = cfgErr.Error()
	}
	a.mu.Lock()
	a.upd = u
	a.mu.Unlock()
}

// startUpdates checks once in the background and then on a fixed cadence.
func (a *App) startUpdates(ctx context.Context) {
	a.mu.Lock()
	u := a.upd
	if u == nil || u.cfgErr != nil || a.closing {
		a.mu.Unlock()
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	a.updCancel = cancel
	a.mu.Unlock()

	go func() {
		a.checkUpdates(loopCtx)
		ticker := time.NewTicker(updateCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				a.checkUpdates(loopCtx)
			}
		}
	}()
}

// UpdateStatus reports the current update state without network I/O.
func (a *App) UpdateStatus() UpdateState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.updateStateLocked()
}

func (a *App) updateStateLocked() UpdateState {
	u := a.upd
	state := UpdateState{Current: Version}
	if u == nil {
		return state
	}
	state.Current = u.version
	state.Supported = u.supported
	state.Configured = u.cfgErr == nil
	if u.cfgErr == nil {
		state.Channel = u.cfg.Channel
		state.Unsigned = u.cfg.Channel != "stable"
	}
	state.Latest = u.manifest.Version
	state.Available = u.available
	state.Mandatory = u.mandatory
	state.Notes = u.manifest.Notes
	state.Checking = u.checking
	state.Downloading = u.downloading
	state.Downloaded = u.downloaded != ""
	state.Installing = u.installing
	state.Progress = u.progress
	state.Error = u.err
	state.Message = u.message
	if u.downloading {
		state.DownloadedSize = u.downBytes
	} else if u.downloaded != "" {
		if info, err := os.Stat(u.downloaded); err == nil {
			state.DownloadedSize = info.Size()
		}
	}
	if !u.checkedAt.IsZero() {
		state.CheckedAt = u.checkedAt.Format(time.RFC3339)
	}
	return state
}

func (a *App) emitUpdate() {
	a.emitEvent("update:changed", a.UpdateStatus())
}

// CheckUpdate performs a manual check. It runs synchronously so the button can
// show the outcome; the background loop uses the same guard.
func (a *App) CheckUpdate() (UpdateState, error) {
	a.mu.Lock()
	u := a.upd
	if u == nil {
		st := a.updateStateLocked()
		a.mu.Unlock()
		return st, errors.New("更新服务尚未初始化")
	}
	cfgErr := u.cfgErr
	checking := u.checking
	a.mu.Unlock()
	if cfgErr != nil {
		return a.UpdateStatus(), nil
	}
	if checking {
		return a.UpdateStatus(), nil
	}
	a.checkUpdates(context.Background(), a.UpdateStatus().Current)
	return a.UpdateStatus(), nil
}

func (a *App) checkUpdates(ctx context.Context, current ...string) {
	a.mu.Lock()
	u := a.upd
	if u == nil || u.cfgErr != nil || u.checking || u.installing || a.closing {
		a.mu.Unlock()
		return
	}
	u.checking = true
	u.err = ""
	client := u.cfg.Client()
	if len(current) > 0 && current[0] != "" {
		u.version = current[0]
	}
	cur := u.version
	a.mu.Unlock()
	a.emitUpdate()

	cctx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	res, err := client.Check(cctx, cur)

	a.mu.Lock()
	u.checking = false
	u.checkedAt = time.Now()
	newVersion := false
	if err != nil {
		u.err = err.Error()
		u.message = ""
	} else {
		newVersion = res.Available && res.Manifest.Version != u.manifest.Version
		u.manifest = res.Manifest
		u.available = res.Available
		u.mandatory = res.Mandatory
		u.err = ""
		if res.Available {
			u.message = ""
		} else {
			u.message = "已是最新版本"
		}
	}
	a.mu.Unlock()
	a.emitUpdate()

	if !newVersion {
		return
	}
	e := model.Event{Level: "info", Code: "update.available"}
	e.Message = fmt.Sprintf("发现新版本 %s（当前 %s），可在“设置 → 更新”中下载。", res.Manifest.Version, cur)
	if res.Mandatory {
		e.Level = "warning"
		e.Code = "update.mandatory"
		e.Message = fmt.Sprintf("必须更新：%s 已不再支持当前版本 %s。", res.Manifest.Version, cur)
	}
	a.addEvent(context.Background(), e)
}

// DownloadUpdate fetches and hash-verifies the installer in the background.
func (a *App) DownloadUpdate() (UpdateState, error) {
	a.mu.Lock()
	u := a.upd
	var err error
	switch {
	case u == nil:
		err = errors.New("更新服务尚未初始化")
	case u.cfgErr != nil:
		err = u.cfgErr
	case !u.available:
		err = errors.New("当前没有可下载的更新")
	case u.downloading:
		err = nil
		st := a.updateStateLocked()
		a.mu.Unlock()
		return st, err
	}
	if err != nil {
		st := a.updateStateLocked()
		a.mu.Unlock()
		return st, err
	}
	u.downloading = true
	u.downloaded = ""
	u.downBytes = 0
	u.progress = 0
	u.err = ""
	u.message = ""
	manifest, cfg := u.manifest, u.cfg
	base := a.baseCtx
	if base == nil {
		base = context.Background()
	}
	a.mu.Unlock()
	a.emitUpdate()

	go func() {
		client := cfg.Client()
		client.OnProgress = func(done, total int64) {
			pct := 0
			if total > 0 {
				pct = int(done * 100 / total)
				if pct > 99 && done < total {
					pct = 99
				}
			}
			a.mu.Lock()
			if u.downloading {
				u.downBytes, u.progress = done, pct
			}
			a.mu.Unlock()
			a.emitEvent("update:progress", map[string]any{
				"progress": pct, "bytes": done, "total": total,
			})
		}
		ctx, cancel := context.WithTimeout(base, updateDownloadTimeout)
		defer cancel()
		path, err := client.Download(ctx, manifest, cfg.Dir)

		a.mu.Lock()
		u.downloading = false
		if err == nil && cfg.Channel == "stable" {
			// Stable installers must be Authenticode-signed by the pinned
			// publisher; checking now keeps an unsigned file from ever being
			// presented as ready to install.
			if verr := updater.VerifyAuthenticode(path, cfg.SignerThumbprint); verr != nil {
				_ = os.Remove(path)
				err = fmt.Errorf("安装包代码签名校验失败：%w", verr)
			}
		}
		if err != nil {
			u.progress = 0
			u.err = err.Error()
		} else {
			u.downloaded = path
			u.progress = 100
			if info, serr := os.Stat(path); serr == nil {
				u.downBytes = info.Size()
			}
			u.message = "更新已下载并通过校验，可以安装。"
		}
		a.mu.Unlock()
		a.emitUpdate()
	}()
	return a.UpdateStatus(), nil
}

// InstallUpdate re-verifies the downloaded installer and hands it to the
// platform launcher, which starts it and closes this application.
func (a *App) InstallUpdate() (UpdateState, error) {
	a.mu.Lock()
	u := a.upd
	var err error
	switch {
	case u == nil:
		err = errors.New("更新服务尚未初始化")
	case u.cfgErr != nil:
		err = u.cfgErr
	case u.installing:
		st := a.updateStateLocked()
		a.mu.Unlock()
		return st, nil
	case u.downloaded == "":
		err = errors.New("请先下载更新")
	case !u.supported:
		err = errors.New("当前为开发构建，无法自动安装更新；请运行正式安装包升级")
	}
	if err != nil {
		if u != nil {
			u.err = err.Error()
		}
		st := a.updateStateLocked()
		a.mu.Unlock()
		return st, err
	}
	path, cfg, manifest := u.downloaded, u.cfg, u.manifest
	launch := a.installer
	a.mu.Unlock()

	ok, herr := updater.HashMatches(path, manifest.SHA256)
	if herr != nil || !ok {
		return a.failInstall("安装包校验失败，请重新下载")
	}
	if cfg.Channel == "stable" {
		if verr := updater.VerifyAuthenticode(path, cfg.SignerThumbprint); verr != nil {
			return a.failInstall("安装包代码签名校验失败：" + verr.Error())
		}
	}
	if launch == nil {
		return a.failInstall("安装程序启动器未就绪")
	}
	if lerr := launch(path); lerr != nil {
		return a.failInstall("启动安装程序失败：" + lerr.Error())
	}

	a.mu.Lock()
	u.installing = true
	u.message = "安装程序已启动，应用即将退出。"
	u.err = ""
	a.mu.Unlock()
	a.emitUpdate()
	return a.UpdateStatus(), nil
}

func (a *App) failInstall(msg string) (UpdateState, error) {
	a.mu.Lock()
	if a.upd != nil {
		a.upd.err = msg
	}
	st := a.updateStateLocked()
	a.mu.Unlock()
	a.emitUpdate()
	return st, errors.New(msg)
}

// SetInstallerLauncher wires the GUI-specific step that starts the installer
// and quits. The headless builds never call it.
func (a *App) SetInstallerLauncher(fn func(path string) error) {
	a.mu.Lock()
	a.installer = fn
	a.mu.Unlock()
}

// consumeRollbackNotice turns the marker left by a crashed build into a
// persistent alert, then removes it.
func (a *App) consumeRollbackNotice(ctx context.Context) {
	n, ok := updater.TakeRollbackNotice(filepath.Join(filepath.Dir(a.dbPath), "update-rollback.json"))
	if !ok {
		return
	}
	a.addEvent(ctx, model.Event{
		Level: "warning", Code: "update.rolled_back",
		Message: fmt.Sprintf("版本 %s 连续启动失败，已自动回退到 %s。请检查运行环境后重新安装该更新。", n.From, n.To),
	})
}
