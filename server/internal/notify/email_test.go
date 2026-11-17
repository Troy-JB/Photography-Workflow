package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildMessageEncodesHeadersAndBody(t *testing.T) {
	raw, err := BuildMessage(Message{
		From:    "studio@example.com",
		To:      "client@example.com",
		Subject: "《婚礼》审片邀请（第 1 轮）",
		Body:    "请在 2026-11-02 18:00 前查看：\nhttps://photos.example.com/#/p/abc\n",
	})
	if err != nil {
		t.Fatalf("build message: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"From: studio@example.com\r\n",
		"To: client@example.com\r\n",
		"Subject: =?utf-8?",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"Content-Transfer-Encoding: base64\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("message missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "https://photos.example.com") {
		t.Fatal("body must be encoded, not sent as plain text")
	}
}

func TestBuildMessageRejectsHeaderInjection(t *testing.T) {
	_, err := BuildMessage(Message{
		From: "studio@example.com", To: "client@example.com",
		Subject: "hi\r\nBcc: evil@example.com", Body: "x",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected invalid input, got %v", err)
	}
	if _, err := BuildMessage(Message{From: "nope", To: "client@example.com", Subject: "s", Body: "b"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected invalid from address, got %v", err)
	}
	if _, err := BuildMessage(Message{From: "studio@example.com", To: "client@example.com", Subject: "", Body: "b"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected missing subject error, got %v", err)
	}
}

func TestClassifySMTP(t *testing.T) {
	if err := classifySMTP(&textproto.Error{Code: 550, Msg: "no such user"}); !errors.Is(err, ErrPermanent) {
		t.Fatalf("550 should be permanent, got %v", err)
	}
	if err := classifySMTP(&textproto.Error{Code: 451, Msg: "try later"}); errors.Is(err, ErrPermanent) {
		t.Fatalf("451 should be retryable, got %v", err)
	}
	if err := classifySMTP(errors.New("dial tcp: i/o timeout")); errors.Is(err, ErrPermanent) {
		t.Fatalf("network error should be retryable, got %v", err)
	}
}

func TestSMTPSenderDeliversToSink(t *testing.T) {
	addr, messages := startSMTPSink(t)
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	sender := NewSMTPSender(EmailConfig{
		Enabled: true, Host: host, Port: port, TLS: "none", Timeout: 5 * time.Second,
		From: "studio@example.com",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sender.Send(ctx, Message{
		From: "studio@example.com", To: "client@example.com",
		Subject: "审片邀请", Body: "https://photos.example.com/#/p/token\n",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-messages:
		if !strings.Contains(got, "Subject: =?utf-8?") {
			t.Fatalf("captured message lacks encoded subject:\n%s", got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for captured message")
	}
}

// startSMTPSink accepts one SMTP session and returns the raw DATA payload.
func startSMTPSink(t *testing.T) (string, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	messages := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		write("220 sink ready")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				write("250-sink")
				write("250 SIZE 10485760")
			case strings.HasPrefix(cmd, "MAIL FROM:"), strings.HasPrefix(cmd, "RCPT TO:"):
				write("250 OK")
			case cmd == "DATA":
				write("354 go ahead")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				messages <- b.String()
				write("250 queued")
			case cmd == "QUIT":
				write("221 bye")
				return
			default:
				write("250 OK")
			}
		}
	}()
	return ln.Addr().String(), messages
}
