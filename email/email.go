// Package email provides SMTP email sending.
package email

import (
	"log/slog"
	"strings"
)

// Config holds SMTP configuration.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

// Sender sends emails.
type Sender struct {
	Config *Config
}

// NewSender creates a new email sender.
func NewSender(cfg *Config) *Sender {
	return &Sender{Config: cfg}
}

// Message represents an email message.
type Message struct {
	To      []string
	Subject string
	Body    string
	IsHTML  bool
}

// Send sends an email message.
// In Phase 1, this logs the email instead of actually sending it.
func (s *Sender) Send(msg *Message) error {
	slog.Info("sending email",
		"from", s.Config.From,
		"to", strings.Join(msg.To, ", "),
		"subject", msg.Subject,
	)
	// In production, this would use net/smtp or a library.
	// Body is not logged to avoid leaking sensitive content (e.g. reset tokens).
	return nil
}

// SendTemplate sends an email using template interpolation.
// Replaces {fieldname} placeholders with values from the data map.
func (s *Sender) SendTemplate(to []string, subject, body string, data map[string]string) error {
	renderedSubject := interpolate(subject, data)
	renderedBody := interpolate(body, data)

	return s.Send(&Message{
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

