package email

import (
	"testing"
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
