package site

import "testing"

func TestLoadStartupConfig_DefaultsConsoleOnboardingDisabled(t *testing.T) {
	t.Setenv("KORA_CONSOLE_ONBOARDING_ENABLED", "")
	cfg := LoadStartupConfig()
	if cfg.AllowConsoleOnboarding {
		t.Fatal("console onboarding should be disabled by default")
	}
}

func TestLoadStartupConfig_ConsoleOnboardingOverrides(t *testing.T) {
	t.Setenv("KORA_CONSOLE_ONBOARDING_ENABLED", "true")
	cfg := LoadStartupConfig()
	if !cfg.AllowConsoleOnboarding {
		t.Fatal("console onboarding should honor env override")
	}
}
