package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/site"
)

var (
	setupDBHost     string
	setupDBPort     int
	setupDBUser     string
	setupDBPass     string
	setupDBName     string
	setupAdminEmail string
	setupAdminPass  string
	setupConfigPath string
)

func init() {
	setupCmd := &cobra.Command{
		Use:   "setup --site <hostname> --path <config_dir>",
		Short: "One-command setup: create DB, import config, migrate, create admin",
		Long: `Set up a complete Kora site from scratch with zero manual SQL.

Creates the database, bootstraps system tables, imports application config,
runs schema migrations, and creates an admin user — all in one command.`,
		RunE: runSetup,
	}

	setupCmd.Flags().StringVar(&setupDBHost, "db-host", "", "MySQL host (default: $KORA_DB_HOST or 127.0.0.1)")
	setupCmd.Flags().IntVar(&setupDBPort, "db-port", 0, "MySQL port (default: $KORA_DB_PORT or 3306)")
	setupCmd.Flags().StringVar(&setupDBUser, "db-user", "", "MySQL user (default: $KORA_DB_USER or root)")
	setupCmd.Flags().StringVar(&setupDBPass, "db-pass", "", "MySQL password")
	setupCmd.Flags().StringVar(&setupDBName, "db-name", "", "Database name (default: derived from site hostname)")
	setupCmd.Flags().StringVar(&setupAdminEmail, "admin-email", "", "Admin user email (required)")
	setupCmd.Flags().StringVar(&setupAdminPass, "admin-password", "", "Admin user password (required)")
	setupCmd.Flags().StringVar(&setupConfigPath, "path", "", "Path to config directory (required)")
	setupCmd.Flags().StringVar(&serveSiteFlag, "site", "", "Site hostname (required)")

	setupCmd.MarkFlagRequired("site")
	setupCmd.MarkFlagRequired("path")
	setupCmd.MarkFlagRequired("admin-email")
	setupCmd.MarkFlagRequired("admin-password")

	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	siteName := serveSiteFlag

	slog.Info("starting setup", "site", siteName, "config", setupConfigPath)

	result, err := site.CreateSite(site.CreateSiteInput{
		Hostname:           siteName,
		DBHost:             setupDBHost,
		DBPort:             setupDBPort,
		DBName:             setupDBName,
		DBUser:             setupDBUser,
		DBPassword:         setupDBPass,
		AdminEmail:         setupAdminEmail,
		AdminPassword:      setupAdminPass,
		PlatformDBHost:     os.Getenv("KORA_DB_HOST"),
		PlatformDBPort:     envIntDefault("KORA_DB_PORT", 0),
		PlatformDBUser:     os.Getenv("KORA_DB_USER"),
		PlatformDBPassword: os.Getenv("KORA_DB_PASSWORD"),
		PlatformDBDSN:      os.Getenv("DB_DSN"),
	})
	if err != nil {
		return fmt.Errorf("creating site: %w", err)
	}
	defer result.DB.Close()

	fmt.Printf("  ✓ Site %s created (database: %s)\n", siteName, result.Config.DBName)
	fmt.Printf("  ✓ Admin user: %s\n", setupAdminEmail)

	// Import YAML config.
	slog.Info("importing config", "path", setupConfigPath)
	if err := site.ImportConfig(result.DB, result.Registry, result.Config.DBName, siteName, setupConfigPath, kdb.Resolve(result.Config.DBType)); err != nil {
		return fmt.Errorf("importing config: %w", err)
	}

	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────┐")
	fmt.Println("│            Kora setup complete!                     │")
	fmt.Printf("│  Site:     %-40s │\n", siteName)
	fmt.Printf("│  Database: %-40s │\n", result.Config.DBName)
	fmt.Printf("│  Admin:    %-40s │\n", setupAdminEmail)
	fmt.Println("│                                                     │")
	fmt.Printf("│  Start: kora serve --site %-25s │\n", siteName)
	fmt.Println("└─────────────────────────────────────────────────────┘")

	return nil
}

func envIntDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		n := 0
		// Parse manually to avoid strconv import.
		for _, c := range v {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		return n
	}
	return fallback
}
