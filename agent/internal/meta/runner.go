package meta

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"photography-workflow/agent/internal/tool"
)

// StayOpen keeps one `exiftool -stay_open` process alive. Calls are serialized
// because ExifTool answers one -execute batch at a time.
type StayOpen struct {
	path string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr bytes.Buffer
	closed bool
}

// Open returns a reader for the given exiftool binary. The process starts on
// the first request and is restarted automatically after a crash or timeout.
func Open(path string) (*StayOpen, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("meta: exiftool path is not configured")
	}
	return &StayOpen{path: path}, nil
}

// JSON runs one `-json -G -n` extraction.
func (s *StayOpen) JSON(ctx context.Context, path string) ([]byte, error) {
	return s.exec(ctx, "-json", "-G", "-n",
		"-charset", "filename=utf8",
		"-d", "%Y-%m-%dT%H:%M:%S",
		path)
}

// Binary extracts the first embedded preview tag with data. These calls are
// one-shot: binary payloads have no reliable delimiter in stay-open mode.
func (s *StayOpen) Binary(ctx context.Context, path string, tags []string) ([]byte, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("meta: extractor is closed")
	}
	for _, tag := range tags {
		cmd := exec.CommandContext(ctx, s.path, "-b", "-"+tag,
			"-charset", "filename=utf8", path)
		tool.HideWindow(cmd)
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			return out, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrNoPreview
}

func (s *StayOpen) exec(ctx context.Context, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("meta: extractor is closed")
	}
	if err := s.start(); err != nil {
		return nil, err
	}
	proc := s.cmd

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			// Reads below unblock once the process dies; the caller turns the
			// error into ctx.Err().
			if proc.Process != nil {
				_ = proc.Process.Kill()
			}
		case <-stop:
		}
	}()

	data, err := s.roundTrip(args)
	if ctx.Err() != nil {
		s.kill()
		return nil, ctx.Err()
	}
	if err != nil {
		s.kill()
		stderr := strings.TrimSpace(s.stderr.String())
		if stderr != "" {
			return nil, fmt.Errorf("meta: exiftool: %w (%s)", err, stderr)
		}
		return nil, fmt.Errorf("meta: exiftool: %w", err)
	}
	return data, nil
}

func (s *StayOpen) start() error {
	if s.cmd != nil {
		return nil
	}
	cmd := exec.Command(s.path, "-stay_open", "True", "-@", "-")
	tool.HideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("meta: exiftool stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("meta: exiftool stdout: %w", err)
	}
	s.stderr.Reset()
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("meta: start exiftool: %w", err)
	}
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReaderSize(stdout, 64<<10)
	return nil
}

func (s *StayOpen) roundTrip(args []string) ([]byte, error) {
	for _, arg := range args {
		if _, err := io.WriteString(s.stdin, arg+"\n"); err != nil {
			return nil, err
		}
	}
	if _, err := io.WriteString(s.stdin, "-execute\n"); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for {
		line, err := s.stdout.ReadBytes('\n')
		if len(line) > 0 {
			if strings.TrimSpace(string(line)) == "{ready}" {
				return out.Bytes(), nil
			}
			out.Write(line)
		}
		if err != nil {
			return nil, err
		}
	}
}

// kill tears down the current process. The caller must hold mu or be exiting.
func (s *StayOpen) kill() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd != nil {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.cmd.Wait()
	}
	s.cmd, s.stdin, s.stdout = nil, nil, nil
}

// Close stops the persistent process.
func (s *StayOpen) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.kill()
	return nil
}
