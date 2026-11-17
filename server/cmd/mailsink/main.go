// Command mailsink is a development-only SMTP sink. It accepts any message,
// writes the raw envelope and body to a directory and never forwards anything.
// It exists so proof email can be exercised end to end without a real server.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:1025", "listen address")
	dir := flag.String("dir", "tmp/mailsink", "directory for captured messages")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Error("create sink directory", "err", err)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "addr", *addr, "err", err)
		os.Exit(1)
	}
	defer ln.Close()
	log.Info("mailsink listening (development only)", "addr", *addr, "dir", *dir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var seq int64
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("accept failed", "err", err)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
			session := &smtpSession{conn: c, dir: *dir, seq: atomic.AddInt64(&seq, 1), log: log}
			session.serve()
		}(conn)
	}
}

type smtpSession struct {
	conn net.Conn
	dir  string
	seq  int64
	log  *slog.Logger

	from     string
	rcpt     []string
	messages int
}

func (s *smtpSession) serve() {
	r := bufio.NewReader(s.conn)
	w := bufio.NewWriter(s.conn)
	write := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format+"\r\n", args...); err != nil {
			return false
		}
		return w.Flush() == nil
	}
	if !write("220-%s ESMTP mailsink", hostname()) || !write("220 ready") {
		return
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		upper := strings.ToUpper(cmd)
		switch {
		case upper == "QUIT":
			write("221 bye")
			return
		case strings.HasPrefix(upper, "EHLO"):
			// Multiline EHLO: the client needs SIZE and 8BITMIME absent is fine.
			if !write("250-%s", hostname()) || !write("250-SIZE 52428800") {
				return
			}
			if !write("250 OK") {
				return
			}
		case strings.HasPrefix(upper, "HELO"):
			write("250 %s", hostname())
		case strings.HasPrefix(upper, "AUTH"):
			write("502 AUTH not supported by mailsink")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.from = strings.Trim(strings.TrimSpace(cmd[len("MAIL FROM:"):]), "<>")
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			to := strings.Trim(strings.TrimSpace(cmd[len("RCPT TO:"):]), "<>")
			s.rcpt = append(s.rcpt, to)
			write("250 OK")
		case upper == "RSET":
			s.from, s.rcpt = "", nil
			write("250 OK")
		case upper == "NOOP":
			write("250 OK")
		case upper == "DATA":
			if len(s.rcpt) == 0 {
				write("503 need RCPT first")
				continue
			}
			if !write("354 end with <CRLF>.<CRLF>") {
				return
			}
			if !s.readData(r) {
				write("554 message rejected")
				continue
			}
			s.from, s.rcpt = "", nil
			write("250 queued")
		default:
			write("500 unrecognised command")
		}
	}
}

// readData captures the message until a lone dot line. Headers (Subject, To)
// are kept verbatim in the file; the body is whatever the client base64-encoded.
func (s *smtpSession) readData(r *bufio.Reader) bool {
	var b strings.Builder
	total := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return false
		}
		if line == ".\r\n" || line == ".\n" {
			break
		}
		// Undo dot-stuffing as required by RFC 5321.
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		total += len(line)
		if total > 50<<20 {
			return false
		}
		b.WriteString(line)
	}
	name := fmt.Sprintf("%s-%04d.eml", time.Now().Format("20060102-150405"), s.seq)
	path := filepath.Join(s.dir, name)
	content := fmt.Sprintf("X-MailSink-From: %s\r\nX-MailSink-To: %s\r\n%s",
		s.from, strings.Join(s.rcpt, ","), b.String())
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		s.log.Error("write captured message", "path", path, "err", err)
		return false
	}
	s.messages++
	// Deliberately no body or link in stdout: captured files are the test input.
	s.log.Info("message captured", "path", path, "from", s.from, "rcpt_count", len(s.rcpt))
	return true
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "mailsink.local"
	}
	return h
}
