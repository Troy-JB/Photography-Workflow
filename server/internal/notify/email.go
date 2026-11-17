package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// JobKind is the jobs.kind the worker executes for queued email.
const JobKind = "email.send"

// SubjectType groups email deliveries under the jobs queue.
const SubjectType = "email_message"

const maxAttempts = 5

const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusFailed  = "failed"
	StatusDead    = "dead"
	StatusSkipped = "skipped"
)

var (
	ErrNotFound     = errors.New("email message not found")
	ErrInvalidInput = errors.New("invalid email input")
	// ErrPermanent marks SMTP replies that will never succeed on retry (bad
	// mailbox, refused recipient). The message goes straight to dead.
	ErrPermanent = errors.New("permanent delivery failure")
)

// EmailConfig mirrors config.Email. Timeout bounds dial + handshake + send.
type EmailConfig struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      string
	Timeout  time.Duration
}

// Message is the wire form handed to a Mailer.
type Message struct {
	From    string
	To      string
	Subject string
	Body    string
}

// Mailer sends one message synchronously. Tests substitute a recorder.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

type SMTPSender struct {
	cfg EmailConfig
}

func NewSMTPSender(cfg EmailConfig) *SMTPSender {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	return &SMTPSender{cfg: cfg}
}

// Send speaks SMTP directly. starttls upgrades an opportunistic connection,
// tls wraps the socket (implicit TLS, usually port 465) and none is limited to
// local sinks outside production (CheckProduction refuses it).
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	payload, err := BuildMessage(msg)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: s.cfg.Timeout}
	tlsCfg := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	if s.cfg.TLS == "tls" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer client.Close()
	if s.cfg.TLS == "starttls" {
		if err := client.StartTLS(tlsCfg); err != nil {
			return classifySMTP(fmt.Errorf("smtp starttls: %w", err))
		}
	}
	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return classifySMTP(fmt.Errorf("smtp auth: %w", err))
		}
	}
	if err := client.Mail(msg.From); err != nil {
		return classifySMTP(fmt.Errorf("smtp mail from: %w", err))
	}
	if err := client.Rcpt(msg.To); err != nil {
		return classifySMTP(fmt.Errorf("smtp rcpt to: %w", err))
	}
	w, err := client.Data()
	if err != nil {
		return classifySMTP(fmt.Errorf("smtp data: %w", err))
	}
	if _, err := w.Write(payload); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return classifySMTP(fmt.Errorf("smtp finish data: %w", err))
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("smtp quit: %w", err)
	}
	return nil
}

// classifySMTP treats 5xx replies as terminal and everything else as retryable.
func classifySMTP(err error) error {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) && protoErr.Code >= 500 {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return err
}

// BuildMessage renders a plain-text UTF-8 mail with an encoded subject and a
// base64 body. Header injection is rejected rather than sanitised.
func BuildMessage(msg Message) ([]byte, error) {
	if strings.ContainsAny(msg.From, "\r\n") || strings.ContainsAny(msg.To, "\r\n") ||
		strings.ContainsAny(msg.Subject, "\r\n") {
		return nil, fmt.Errorf("%w: header contains a line break", ErrInvalidInput)
	}
	if _, err := mail.ParseAddress(msg.From); err != nil {
		return nil, fmt.Errorf("%w: from address is invalid", ErrInvalidInput)
	}
	if _, err := mail.ParseAddress(msg.To); err != nil {
		return nil, fmt.Errorf("%w: to address is invalid", ErrInvalidInput)
	}
	if strings.TrimSpace(msg.Subject) == "" {
		return nil, fmt.Errorf("%w: subject is required", ErrInvalidInput)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", msg.From)
	fmt.Fprintf(&b, "To: %s\r\n", msg.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", uuid.NewString(), messageDomain(msg.From))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")

	body := msg.Body
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded)
	b.WriteString("\r\n")
	return []byte(b.String()), nil
}

func messageDomain(from string) string {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return "localhost"
	}
	if i := strings.LastIndex(addr.Address, "@"); i >= 0 && i+1 < len(addr.Address) {
		return addr.Address[i+1:]
	}
	return "localhost"
}

// Outgoing describes one row of the email queue. Status defaults to pending;
// skipped rows are used for messages that would have been sent but could not
// be (missing recipient, disabled channel).
type Outgoing struct {
	TenantID    uuid.UUID
	ToEmail     string
	Subject     string
	Body        string
	DedupeKey   string
	RelatedType string
	RelatedID   *uuid.UUID
	Payload     map[string]any
	Status      string
	LastError   string
}

// Enqueue inserts one email in the caller's transaction. A dedupe key hit keeps
// the existing row and returns its id instead of queueing a duplicate.
func Enqueue(ctx context.Context, tx pgx.Tx, o Outgoing) (uuid.UUID, error) {
	to := strings.TrimSpace(o.ToEmail)
	status := o.Status
	if status == "" {
		status = StatusPending
	}
	if status == StatusPending {
		if _, err := mail.ParseAddress(to); err != nil {
			return uuid.Nil, fmt.Errorf("%w: to_email is invalid", ErrInvalidInput)
		}
		if strings.TrimSpace(o.Subject) == "" {
			return uuid.Nil, fmt.Errorf("%w: subject is required", ErrInvalidInput)
		}
		if strings.TrimSpace(o.Body) == "" {
			return uuid.Nil, fmt.Errorf("%w: body is required", ErrInvalidInput)
		}
	}
	payload := []byte("{}")
	if len(o.Payload) > 0 {
		raw, err := json.Marshal(o.Payload)
		if err != nil {
			return uuid.Nil, fmt.Errorf("marshal email payload: %w", err)
		}
		payload = raw
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO email_messages
			(tenant_id, to_email, subject, body_text, status, last_error, dedupe_key,
			 related_type, related_id, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		o.TenantID, to, o.Subject, o.Body, status, o.LastError, o.DedupeKey,
		o.RelatedType, o.RelatedID, payload).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || o.DedupeKey == "" {
		return uuid.Nil, fmt.Errorf("enqueue email: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT id FROM email_messages
		WHERE tenant_id = $1 AND dedupe_key = $2`, o.TenantID, o.DedupeKey).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("load deduped email: %w", err)
	}
	return id, nil
}
