// Package email provides SMTP email sending.
package email

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds SMTP configuration.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// TLSMode controls transport security.
	// Supported values: auto, implicit, starttls, plain.
	TLSMode string
}

// Sender sends emails.
type Sender struct {
	Config *Config
}

// Configured reports whether this sender has an SMTP host. A sender without
// one logs messages for local development and cannot deliver invitations.
func (s *Sender) Configured() bool {
	return s != nil && s.Config != nil && strings.TrimSpace(s.Config.Host) != ""
}

// NewSender creates a new email sender.
func NewSender(cfg *Config) *Sender {
	return &Sender{Config: cfg}
}

// Message represents an email message.
type Message struct {
	To       []string
	Subject  string
	Body     string
	TextBody string
	HTMLBody string
	IsHTML   bool
}

// Send sends an email message.
// Falls back to logging only when no SMTP host is configured.
func (s *Sender) Send(msg *Message) error {
	return s.SendContext(context.Background(), msg)
}

// SendContext sends a message and closes the SMTP connection if ctx is
// canceled. The deadline bounds the complete SMTP operation, not only dial.
func (s *Sender) SendContext(ctx context.Context, msg *Message) error {
	if ctx == nil {
		return fmt.Errorf("email send requires a context")
	}
	if msg == nil {
		return fmt.Errorf("email message is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.Config == nil {
		return fmt.Errorf("email sender is not configured")
	}

	from := strings.TrimSpace(s.Config.From)
	if from == "" {
		from = strings.TrimSpace(s.Config.Username)
	}
	if from == "" {
		from = "kora@localhost"
	}

	if strings.TrimSpace(s.Config.Host) == "" {
		slog.Info("email transport not configured; logging mail instead",
			"from", from,
			"to", strings.Join(msg.To, ", "),
			"subject", msg.Subject,
		)
		if os.Getenv("KORA_EMAIL_LOG_BODY") == "true" {
			slog.Info("logged email body", "to", strings.Join(msg.To, ", "), "body", msg.TextBody)
		}
		return nil
	}
	if len(msg.To) == 0 {
		return fmt.Errorf("email message has no recipients")
	}

	if err := sendSMTPContext(ctx, s.Config, from, msg.To, buildMessage(from, msg)); err != nil {
		return err
	}

	slog.Info("sent email",
		"from", from,
		"to", strings.Join(msg.To, ", "),
		"subject", msg.Subject,
		"host", s.Config.Host,
		"port", smtpPort(s.Config.Port),
	)
	return nil
}

// SendTemplate sends an email using template interpolation.
// Replaces {fieldname} placeholders with values from the data map.
func (s *Sender) SendTemplate(to []string, subject, body string, data map[string]string) error {
	return s.SendTemplateContext(context.Background(), to, subject, body, data)
}

// SendTemplateContext renders and sends a template under the caller's
// cancellation and deadline.
func (s *Sender) SendTemplateContext(ctx context.Context, to []string, subject, body string, data map[string]string) error {
	renderedSubject := interpolate(subject, data)
	renderedBody := interpolate(body, data)

	return s.SendContext(ctx, &Message{
		To:      to,
		Subject: renderedSubject,
		Body:    renderedBody,
	})
}

func interpolate(template string, data map[string]string) string {
	result := template
	for key, val := range data {
		result = strings.ReplaceAll(result, "{"+key+"}", val)
	}
	return result
}

func smtpPort(port int) int {
	if port > 0 {
		return port
	}
	return 587
}

func buildMessage(from string, msg *Message) []byte {
	textBody := msg.TextBody
	if textBody == "" {
		textBody = msg.Body
	}
	htmlBody := msg.HTMLBody
	if htmlBody == "" && msg.IsHTML {
		htmlBody = msg.Body
	}

	if htmlBody != "" {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		boundary := w.Boundary()
		_, _ = fmt.Fprintf(&buf, "From: %s\r\n", from)
		_, _ = fmt.Fprintf(&buf, "To: %s\r\n", strings.Join(msg.To, ", "))
		_, _ = fmt.Fprintf(&buf, "Subject: %s\r\n", msg.Subject)
		_, _ = fmt.Fprint(&buf, "MIME-Version: 1.0\r\n")
		_, _ = fmt.Fprintf(&buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)

		if textBody == "" {
			textBody = stripHTML(htmlBody)
		}

		_ = writeMultipartPart(w, "text/plain; charset=UTF-8", textBody)
		_ = writeMultipartPart(w, "text/html; charset=UTF-8", htmlBody)
		_ = w.Close()
		return buf.Bytes()
	}

	contentType := "text/plain; charset=UTF-8"
	headers := []string{
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", strings.Join(msg.To, ", ")),
		fmt.Sprintf("Subject: %s", msg.Subject),
		"MIME-Version: 1.0",
		fmt.Sprintf("Content-Type: %s", contentType),
		"",
		msg.Body,
	}
	return []byte(strings.Join(headers, "\r\n"))
}

func writeMultipartPart(w *multipart.Writer, contentType, body string) error {
	part, err := w.CreatePart(textProtoHeader(contentType))
	if err != nil {
		return err
	}
	_, err = part.Write([]byte(body))
	return err
}

func textProtoHeader(contentType string) textproto.MIMEHeader {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", contentType)
	return h
}

func stripHTML(input string) string {
	replacer := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n")
	return replacer.Replace(input)
}

func sendSMTPContext(ctx context.Context, cfg *Config, from string, to []string, data []byte) (resultErr error) {
	host := strings.TrimSpace(cfg.Host)
	port := smtpPort(cfg.Port)
	mode := strings.ToLower(strings.TrimSpace(cfg.TLSMode))
	if mode == "" || mode == "auto" {
		if port == 465 {
			mode = "implicit"
		} else {
			mode = "starttls"
		}
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	defer func() {
		if err := opCtx.Err(); err != nil {
			resultErr = err
		}
	}()

	var client *smtp.Client
	var conn net.Conn
	var err error

	switch mode {
	case "implicit":
		conn, err = (&net.Dialer{}).DialContext(opCtx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("dialing smtp tls: %w", err)
		}
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(opCtx); err != nil {
			_ = conn.Close()
			return fmt.Errorf("smtp tls handshake: %w", err)
		}
		conn = tlsConn
	case "plain", "starttls":
		conn, err = (&net.Dialer{}).DialContext(opCtx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("dialing smtp: %w", err)
		}
	default:
		return fmt.Errorf("unsupported smtp tls mode %q", cfg.TLSMode)
	}
	stopClose := context.AfterFunc(opCtx, func() { _ = conn.Close() })
	defer stopClose()
	client, err = smtp.NewClient(conn, host)
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		return fmt.Errorf("creating smtp client: %w", err)
	}
	defer client.Close() //nolint:errcheck // QUIT is sent explicitly; this only releases the socket

	if err := client.Hello("localhost"); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}

	if mode == "starttls" {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return fmt.Errorf("smtp server does not support STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}

	username := strings.TrimSpace(cfg.Username)
	password := cfg.Password
	if username != "" || password != "" {
		auth := smtp.PlainAuth("", username, password, host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("smtp rcpt to %s: %w", recipient, err)
		}
	}

	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := wc.Write(data); err != nil {
		_ = wc.Close()
		return fmt.Errorf("smtp write data: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}

	return nil
}
