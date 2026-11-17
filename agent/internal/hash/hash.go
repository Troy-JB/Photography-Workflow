// Package hash computes the content SHA-256 the agent uses as the authority
// for copy verification and deduplication.
package hash

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"

	"photography-workflow/agent/internal/fsx"
)

// BufferSize is the copy/read chunk used across the agent.
const BufferSize = 1 << 20

// Result describes one hashed stream.
type Result struct {
	SHA256 string
	Bytes  int64
}

// Hash is a streaming SHA-256 that also counts bytes passed through it.
type Hash struct {
	h hash.Hash
	n int64
}

func New() *Hash { return &Hash{h: sha256.New()} }

func (h *Hash) Write(p []byte) (int, error) {
	n, err := h.h.Write(p)
	h.n += int64(n)
	return n, err
}

func (h *Hash) SHA256() string { return hex.EncodeToString(h.h.Sum(nil)) }

func (h *Hash) Bytes() int64 { return h.n }

// File hashes an on-disk file, applying Windows long-path handling.
func File(ctx context.Context, path string) (Result, error) {
	f, err := os.Open(fsx.Local(path))
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return Reader(ctx, f)
}

// Reader hashes r, aborting promptly when ctx is cancelled.
func Reader(ctx context.Context, r io.Reader) (Result, error) {
	h := New()
	buf := make([]byte, BufferSize)
	if _, err := io.CopyBuffer(h, &ctxReader{ctx: ctx, r: r}, buf); err != nil {
		return Result{}, err
	}
	return Result{SHA256: h.SHA256(), Bytes: h.Bytes()}, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
