//go:build wails

package binder

import (
	"context"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// PickDirectory opens the native folder chooser. It only exists in GUI builds;
// the frontend always offers a plain text input as a fallback.
func (a *App) PickDirectory(title, startDir string) (string, error) {
	a.mu.Lock()
	ctx := a.baseCtx
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return runtime.OpenDirectoryDialog(ctx, runtime.OpenDialogOptions{
		Title:                title,
		DefaultDirectory:     startDir,
		CanCreateDirectories: true,
	})
}
