package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"photography-workflow/server/internal/storage"
)

// Provider names. manual keeps retouched files in the admin uploader;
// folder_watch trades them through shared directories. ftp, pixcake and
// xiangxiang are interface-only placeholders: selecting one fails at startup so
// a deployment can never believe a channel works when it does not.
const (
	ProviderManual      = "manual"
	ProviderFolderWatch = "folder_watch"
	ProviderFTP         = "ftp"
	ProviderPixCake     = "pixcake"
	ProviderXiangxiang  = "xiangxiang"
)

// ErrProviderNotImplemented is returned for vendors that only have an interface
// stub. It is a startup error by design.
var ErrProviderNotImplemented = errors.New("retouch provider is only an interface stub")

type Config struct {
	Provider     string
	Root         string
	OverdueAfter time.Duration
}

// Task is the material a provider needs to export one retouch job. Items point
// at the stored originals; the provider copies them and writes a manifest.
type Task struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	RoundID   uuid.UUID
	RoundNo   int
	StartedAt time.Time
	Items     []TaskItem
}

type TaskItem struct {
	AssetID   uuid.UUID
	Filename  string
	Position  int
	Ext       string
	SourceKey string
}

// Import is one file a provider found for a task. The service verifies and
// records it as an asset revision.
type Import struct {
	TaskID   uuid.UUID
	AssetID  uuid.UUID
	Path     string
	Filename string
	Size     int64
}

type Provider interface {
	Name() string
	// Export hands the task to the outside world. It returns an opaque
	// reference for audit and must be safe to retry.
	Export(ctx context.Context, task Task) (string, error)
	// Poll lists files the outside world has handed back. It is called on every
	// sweep; processed files must be moved away so they are not returned again.
	Poll(ctx context.Context) ([]Import, error)
}

// New builds the configured provider. An unimplemented vendor is a startup
// error rather than a silent no-op.
func New(cfg Config, store storage.Storage) (Provider, error) {
	name := strings.ToLower(strings.TrimSpace(cfg.Provider))
	switch name {
	case "", ProviderManual:
		return manualProvider{}, nil
	case ProviderFolderWatch:
		root := strings.TrimSpace(cfg.Root)
		if root == "" {
			return nil, fmt.Errorf("%w: folder_watch needs a root directory", ErrProviderNotImplemented)
		}
		return NewFolderWatch(root, store), nil
	case ProviderFTP, ProviderPixCake, ProviderXiangxiang:
		return nil, fmt.Errorf("%w: %s has no adapter yet; wire the vendor integration before enabling it",
			ErrProviderNotImplemented, name)
	default:
		return nil, fmt.Errorf("unknown retouch provider %q", name)
	}
}

// manualProvider tracks nothing outside the database; the admin uploader calls
// the revision import endpoint directly.
type manualProvider struct{}

func (manualProvider) Name() string { return ProviderManual }

func (manualProvider) Export(context.Context, Task) (string, error) { return "", nil }

func (manualProvider) Poll(context.Context) ([]Import, error) { return nil, nil }
