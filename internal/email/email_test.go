package email

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNewSender(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
	}{
		{
			name: "FullConfig",
			cfg: &Config{
				Host:     "smtp.example.com",
				Port:     587,
				Username: "user",
				Password: "pass",
				From:     "noreply@example.com",
			},
		},
		{
			name: "MinimalConfig",
			cfg: &Config{
				From: "kora@localhost",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSender(tt.cfg)
			if s.Config != tt.cfg {
				t.Error("NewSender should store the config")
			}
		})
	}
}

func TestTemplateRender_Simple(t *testing.T) {
	s := NewSender(&Config{From: "test@test.com"})
	err := s.SendTemplate(
		[]string{"user@test.com"},
		"Hello {name}",
		"Welcome {name}!",
		map[string]string{"name": "Alice"},
	)
	if err != nil {
		t.Errorf("SendTemplate should not error: %v", err)
	}
}

func TestTemplateRender_MultipleVars(t *testing.T) {
	s := NewSender(&Config{From: "test@test.com"})
	err := s.SendTemplate(
		[]string{"user@test.com"},
		"Order {order_id} for {customer}",
		"Dear {customer},\n\nYour order {order_id} is confirmed.",
		map[string]string{
			"order_id": "ORD-123",
			"customer": "Bob",
		},
	)
	if err != nil {
		t.Errorf("SendTemplate should not error: %v", err)
	}
}

func TestTemplateRender_NoPlaceholders(t *testing.T) {
	s := NewSender(&Config{From: "test@test.com"})
	err := s.SendTemplate(
		[]string{"user@test.com"},
		"Plain Subject",
		"Plain body with no variables.",
		nil,
	)
	if err != nil {
		t.Errorf("SendTemplate should not error: %v", err)
	}
}

func TestTemplateRender_MissingVar(t *testing.T) {
	s := NewSender(&Config{From: "test@test.com"})
	// When data is nil, placeholders remain as-is — no crash.
	err := s.SendTemplate(
		[]string{"user@test.com"},
		"Hello {name}",
		"Welcome {name}!",
		nil,
	)
	if err != nil {
		t.Errorf("SendTemplate should not error with nil data: %v", err)
	}
}

func TestConfig_Defaults(t *testing.T) {
	cfg := &Config{
		Host: "localhost",
		Port: 1025,
		From: "kora@test.local",
	}
	if cfg.Host != "localhost" {
		t.Errorf("Host = %q, want %q", cfg.Host, "localhost")
	}
	if cfg.Port != 1025 {
		t.Errorf("Port = %d, want %d", cfg.Port, 1025)
	}
	if cfg.From != "kora@test.local" {
		t.Errorf("From = %q, want %q", cfg.From, "kora@test.local")
	}
}

func TestInterpolate(t *testing.T) {
	tests := []struct {
		name     string
		tmpl     string
		data     map[string]string
		expected string
	}{
		{
			name:     "simple substitution",
			tmpl:     "Hello {name}",
			data:     map[string]string{"name": "World"},
			expected: "Hello World",
		},
		{
			name:     "multiple substitutions",
			tmpl:     "{a} and {b}",
			data:     map[string]string{"a": "1", "b": "2"},
			expected: "1 and 2",
		},
		{
			name:     "no match stays literal",
			tmpl:     "Hello {name}",
			data:     nil,
			expected: "Hello {name}",
		},
		{
			name:     "empty template",
			tmpl:     "",
			data:     map[string]string{"x": "y"},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := interpolate(tt.tmpl, tt.data)
			if got != tt.expected {
				t.Errorf("interpolate(%q, %v) = %q, want %q", tt.tmpl, tt.data, got, tt.expected)
			}
		})
	}
}

func TestSend_WithPlainSMTPServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- ""
			return
		}
		defer conn.Close()

		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		w := func(s string) {
			_, _ = fmt.Fprint(conn, s)
		}

		w("220 localhost ESMTP\r\n")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO ") || strings.HasPrefix(line, "HELO "):
				w("250-localhost\r\n250 OK\r\n")
			case strings.HasPrefix(line, "MAIL FROM:"):
				w("250 OK\r\n")
			case strings.HasPrefix(line, "RCPT TO:"):
				w("250 OK\r\n")
			case line == "DATA":
				w("354 End data with <CR><LF>.<CR><LF>\r\n")
				inData = true
			case inData && line == ".":
				w("250 OK\r\n")
				done <- data.String()
			case inData:
				data.WriteString(line)
				data.WriteString("\n")
			case line == "QUIT":
				w("221 Bye\r\n")
				return
			default:
				w("250 OK\r\n")
			}
		}
		done <- data.String()
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	s := NewSender(&Config{
		Host:    "127.0.0.1",
		Port:    port,
		From:    "noreply@example.com",
		TLSMode: "plain",
	})
	err = s.Send(&Message{
		To:      []string{"recipient@example.com"},
		Subject: "Hello",
		Body:    "Magic link body",
	})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}

	select {
	case got := <-done:
		if !strings.Contains(got, "Subject: Hello") {
			t.Fatalf("expected message to contain subject, got %q", got)
		}
		if !strings.Contains(got, "Magic link body") {
			t.Fatalf("expected message body, got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for smtp server")
	}
}

func TestSendContextCancellationClosesSMTPConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen:", err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		var b [1]byte
		_, _ = conn.Read(b[:]) // SMTP greeting is withheld until cancellation.
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	sender := NewSender(&Config{Host: listener.Addr().(*net.TCPAddr).IP.String(), Port: listener.Addr().(*net.TCPAddr).Port, TLSMode: "plain"})
	go func() {
		result <- sender.SendContext(ctx, &Message{To: []string{"recipient@example.test"}, Subject: "Action required", Body: "Review your inbox."})
	}()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP connection was not accepted")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SendContext error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendContext did not stop after cancellation")
	}
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled SMTP connection remained open")
	}
}

func TestSendTemplateContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sender := NewSender(&Config{From: "kora@example.test"})
	err := sender.SendTemplateContext(ctx, []string{"recipient@example.test"}, "Hello {name}", "Hi {name}", map[string]string{"name": "Ari"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SendTemplateContext error = %v, want context.Canceled", err)
	}
}

func TestMagicLinkTemplate(t *testing.T) {
	subject, textBody, htmlBody := MagicLinkTemplate("Kigae", "https://example.com/link", 15)
	if !strings.Contains(subject, "Kigae") {
		t.Fatalf("expected subject to mention app name, got %q", subject)
	}
	if !strings.Contains(textBody, "https://example.com/link") {
		t.Fatalf("expected text body to contain link, got %q", textBody)
	}
	if !strings.Contains(htmlBody, "Sign in to Kigae") {
		t.Fatalf("expected html body to contain heading, got %q", htmlBody)
	}
}
