package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("object not found")

type Part struct {
	Number int32  `json:"part_number"`
	ETag   string `json:"etag"`
}

type PresignedPart struct {
	PartNumber int32  `json:"part_number"`
	URL        string `json:"url"`
}

type MultipartUpload struct {
	UploadID  string          `json:"upload_id"`
	ObjectKey string          `json:"object_key"`
	PartSize  int64           `json:"part_size"`
	Parts     []PresignedPart `json:"parts"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// NamedPresigner is an optional capability of storage backends that can force
// a download file name on a signed URL. Callers fall back to PresignGet when a
// backend does not implement it.
type NamedPresigner interface {
	PresignGetNamed(ctx context.Context, key, filename string, expires time.Duration) (string, error)
}

// Storage abstracts object storage so OSS/COS/MinIO can be swapped.
type Storage interface {
	EnsureBucket(ctx context.Context) error
	Health(ctx context.Context) error
	PresignPut(ctx context.Context, key string, expires time.Duration) (string, error)
	// PresignGet signs a short-lived download. Galleries never serve image
	// bytes through the API, so this is the only way customers see images.
	PresignGet(ctx context.Context, key string, expires time.Duration) (string, error)
	// Put writes a server-generated object (derivatives, manifests).
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	CreateMultipart(ctx context.Context, key string) (string, error)
	PresignPart(ctx context.Context, key, uploadID string, partNumber int32, expires time.Duration) (string, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
	Stat(ctx context.Context, key string) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes a server-written object. It is used to roll back a stored
	// file when the transaction that would reference it fails.
	Delete(ctx context.Context, key string) error
	PublicURL(key string) string
}
