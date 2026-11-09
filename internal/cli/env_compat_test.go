package cli

import (
	"testing"

	"github.com/asenawritescode/kora/internal/site"
	"github.com/spf13/cobra"
)

// TestExecuteBootsConfigFromZanaEnvAlone runs the real CLI entry point with
// only ZANA_* variables set and checks that the configuration loaders used by
// `serve` see them.
func TestExecuteBootsConfigFromZanaEnvAlone(t *testing.T) {
	for _, name := range []string{
		"DB_TYPE", "DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "HTTP_PORT",
		"STORAGE_BACKEND", "STORAGE_S3_USE_SSL", "TRUSTED_PROXIES",
	} {
		t.Setenv("KORA_"+name, "")
		t.Setenv("ZANA_"+name, "")
	}
	t.Setenv("ZANA_DB_TYPE", "libsql")
	t.Setenv("ZANA_DB_HOST", "libsql.internal")
	t.Setenv("ZANA_DB_PORT", "9999")
	t.Setenv("ZANA_DB_USER", "zana-user")
	t.Setenv("ZANA_DB_PASSWORD", "zana-pass")
	t.Setenv("ZANA_HTTP_PORT", "8123")
	t.Setenv("ZANA_STORAGE_BACKEND", "s3")
	t.Setenv("ZANA_STORAGE_S3_USE_SSL", "false")

	var gotCommon *site.CommonConfig
	var gotBackend string
	var gotSSL bool
	probe := &cobra.Command{
		Use:    "zana-env-probe",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			gotCommon = site.CommonConfigFromEnv()
			storageCfg := storageConfigFromEnv()
			gotBackend, gotSSL = storageCfg.Backend, storageCfg.S3UseSSL
			return nil
		},
	}
	rootCmd.AddCommand(probe)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(probe)
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs([]string{"zana-env-probe"})

	if err := Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotCommon == nil {
		t.Fatal("probe command did not run")
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"DBType", gotCommon.DBType, "libsql"},
		{"DBHost", gotCommon.DBHost, "libsql.internal"},
		{"DBPort", gotCommon.DBPort, 9999},
		{"DBUser", gotCommon.DBUser, "zana-user"},
		{"DBPassword", gotCommon.DBPassword, "zana-pass"},
		{"HTTPPort", gotCommon.HTTPPort, 8123},
		{"storage backend", gotBackend, "s3"},
		{"storage s3 use ssl", gotSSL, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestExecuteZanaWinsOverKora checks precedence through the same entry point.
func TestExecuteZanaWinsOverKora(t *testing.T) {
	t.Setenv("KORA_DB_HOST", "old-host")
	t.Setenv("ZANA_DB_HOST", "new-host")

	var got string
	probe := &cobra.Command{
		Use:    "zana-env-probe",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			got = site.CommonConfigFromEnv().DBHost
			return nil
		},
	}
	rootCmd.AddCommand(probe)
	t.Cleanup(func() {
		rootCmd.RemoveCommand(probe)
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs([]string{"zana-env-probe"})

	if err := Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got != "new-host" {
		t.Errorf("DBHost = %q, want new-host", got)
	}
}
