//go:build wails

// Command pwf-agent-desktop is the Wails GUI entry point. It is built with the
// `wails` tag because frontend/dist is a build artifact (npm run build creates
// it); the headless CLI in cmd/pwf-agent has no GUI dependency.
package main

import (
	"context"
	"embed"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"photography-workflow/agent/internal/appdir"
	"photography-workflow/agent/internal/binder"
	"photography-workflow/agent/internal/updater"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Update guard: a build that fails to reach an interactive window twice
	// in a row is rolled back to the previous installed version. Development
	// layouts (build/bin) are not versioned and skip this entirely.
	exe, exeErr := os.Executable()
	if exeErr != nil {
		log.Printf("locate executable: %v", exeErr)
	}
	dataDir := appdir.DataDir()
	startState := filepath.Join(dataDir, "update-start.json")
	rollbackNotice := filepath.Join(dataDir, "update-rollback.json")
	root, version, installed := updater.InstallRoot(exe)
	if installed {
		prev, err := updater.BeginStart(startState, root, version)
		if err != nil {
			log.Printf("update guard: %v", err)
		} else if prev != "" {
			if err := updater.WriteCurrent(root, prev); err != nil {
				log.Printf("rollback switch failed: %v", err)
			} else {
				_ = updater.WriteRollbackNotice(rollbackNotice, version, prev)
				// Give the Wails teardown a moment so the two windows do not
				// overlap visibly.
				time.Sleep(800 * time.Millisecond)
				if err := relaunch(root); err != nil {
					log.Printf("rollback relaunch failed: %v", err)
				}
				return
			}
		}
	}

	var (
		ctxMu  sync.RWMutex
		appCtx context.Context
	)
	emit := func(name string, data any) {
		ctxMu.RLock()
		ctx := appCtx
		ctxMu.RUnlock()
		if ctx == nil {
			return
		}
		runtime.EventsEmit(ctx, name, data)
	}

	app := binder.New(appdir.DBPath(), appdir.SettingsPath(), emit)
	app.SetInstallerLauncher(func(path string) error {
		cmd := exec.Command(path)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() {
			// Let the UI show "安装程序已启动" before the window closes; the
			// installer runs in a new version directory, so it does not need
			// this process to exit first.
			time.Sleep(2500 * time.Millisecond)
			ctxMu.RLock()
			ctx := appCtx
			ctxMu.RUnlock()
			if ctx != nil {
				runtime.Quit(ctx)
			}
		}()
		return nil
	})

	err := wails.Run(&options.App{
		Title:     "PWF Agent",
		Width:     1100,
		Height:    680,
		MinWidth:  900,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 15, G: 17, B: 21, A: 1},
		OnStartup: func(ctx context.Context) {
			ctxMu.Lock()
			appCtx = ctx
			ctxMu.Unlock()
			app.Startup(ctx)
			// Recenter after the DPI scale is known; some hosts otherwise
			// cascade the window partly off a small screen.
			runtime.WindowCenter(ctx)
		},
		// The UI is interactive once the DOM is ready; only then is the new
		// version considered healthy and the crash counter cleared.
		OnDomReady: func(ctx context.Context) {
			if !installed {
				return
			}
			if err := updater.MarkHealthy(startState, version); err != nil {
				log.Printf("mark healthy: %v", err)
			}
			// Keep the current and previous versions; older ones only cost
			// disk. Failures (for example a locked file) are not fatal.
			go func() {
				if _, err := updater.PruneVersions(root, version, 1); err != nil {
					log.Printf("prune old versions: %v", err)
				}
			}()
		},
		OnShutdown: app.Shutdown,
		Bind:       []interface{}{app},
		// Keep WebView2's fraudulent-site detection on; Wails otherwise adds a
		// --disable-features argument, which also suppresses
		// WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS used by UI test harnesses.
		EnableFraudulentWebsiteDetection: true,
	})
	if err != nil {
		log.Fatalf("wails: %v", err)
	}
}

// relaunch starts the launcher (or, when it is missing, the executable named
// by current.txt) so the recovered version opens normally.
func relaunch(root string) error {
	launcher := filepath.Join(root, "launcher.exe")
	if _, err := os.Stat(launcher); err != nil {
		target, terr := updater.Target(root)
		if terr != nil {
			return terr
		}
		launcher = target
	}
	return exec.Command(launcher).Start()
}
