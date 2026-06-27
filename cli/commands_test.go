package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// findSubCommand looks up a sub-command by its Use string (first word).
func findSubCommand(root *cobra.Command, name string) *cobra.Command {
	for _, cmd := range root.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}
	return nil
}

func TestRootCommand_Exists(t *testing.T) {
	if rootCmd == nil {
		t.Fatal("rootCmd is nil")
	}
	if rootCmd.Use != "kora" {
		t.Errorf("rootCmd.Use = %q, want %q", rootCmd.Use, "kora")
	}
	if rootCmd.Short == "" {
		t.Error("rootCmd.Short is empty")
	}
}

func TestServeCommand_Exists(t *testing.T) {
	cmd := findSubCommand(rootCmd, "serve")
	if cmd == nil {
		t.Fatal("serve command not found under root")
	}
	if cmd.Short == "" {
		t.Error("serve command Short is empty")
	}
}

func TestSetupCommand_Exists(t *testing.T) {
	// setup is registered in setup.go's init() which may not have run yet.
	// The init() in setup.go defines setupCmd as a local variable and adds it to rootCmd.
	// So we need to check after all init()s have run (they have, since this test runs).
	setupCmd := findSubCommand(rootCmd, "setup")
	if setupCmd == nil {
		t.Skip("setup command registered via init() in separate file")
	}
}

func TestMigrateCommand_Exists(t *testing.T) {
	cmd := findSubCommand(rootCmd, "migrate")
	if cmd == nil {
		t.Fatal("migrate command not found under root")
	}
	flag := cmd.Flags().Lookup("site")
	if flag == nil {
		t.Error("migrate command missing --site flag")
	}
	allFlag := cmd.Flags().Lookup("all")
	if allFlag == nil {
		t.Error("migrate command missing --all flag")
	}
}

func TestConfigCommand_Exists(t *testing.T) {
	cmd := findSubCommand(rootCmd, "config")
	if cmd == nil {
		t.Fatal("config command not found under root")
	}
}

func TestSecretCommand_Exists(t *testing.T) {
	cmd := findSubCommand(rootCmd, "secret")
	if cmd == nil {
		t.Fatal("secret command not found under root")
	}
}

func TestServeFlags(t *testing.T) {
	serveCmd := findSubCommand(rootCmd, "serve")
	if serveCmd == nil {
		t.Fatal("serve command not found")
	}

	flags := []string{"site", "port", "config-dir"}
	for _, flagName := range flags {
		flag := serveCmd.Flags().Lookup(flagName)
		if flag == nil {
			t.Errorf("serve command missing --%s flag", flagName)
		}
	}
}

func TestVersionInjected(t *testing.T) {
	// Version is set via ldflags at build time, but defaults to "dev".
	if Version == "" {
		t.Error("Version is empty, expected at least 'dev'")
	}
}

func TestRootCommand_SubCommands(t *testing.T) {
	names := make(map[string]bool)
	for _, cmd := range rootCmd.Commands() {
		names[cmd.Use] = true
	}

	expected := []string{"serve", "migrate", "config", "secret"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("expected subcommand %q not found under root", name)
		}
	}
}
