package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/site"
)

var (
	setupDBHost         string
	setupDBPort         int
	setupDBUser         string
	setupDBPass         string
	setupDBName         string
	setupAdminEmail     string
	setupCloudSubjectID string
	setupConfigPath     string
	setupSiteName       string
)

func init() {
	setupCmd := &cobra.Command{
		Use:   "setup --site <hostname> --path <config_dir>",
		Short: "One-command setup: create DB, import config, migrate, create admin",
		Long: `Set up a complete Kigae site from scratch with zero manual SQL.

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
	setupCmd.Flags().StringVar(&setupCloudSubjectID, "cloud-subject-id", "", "Stable Cloud user subject ID (required)")
	setupCmd.Flags().StringVar(&setupConfigPath, "path", "", "Path to config directory (required)")
	setupCmd.Flags().StringVar(&setupSiteName, "site", "", "Site hostname (required)")

	setupCmd.MarkFlagRequired("site")
	setupCmd.MarkFlagRequired("path")
	setupCmd.MarkFlagRequired("admin-email")
	setupCmd.MarkFlagRequired("cloud-subject-id")

	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	siteName := setupSiteName

	slog.Info("starting setup", "site", siteName, "config", setupConfigPath)

	createInput := site.CreateSiteInput{
		Hostname:           siteName,
		DBHost:             setupDBHost,
		DBPort:             setupDBPort,
		DBName:             setupDBName,
		DBUser:             setupDBUser,
		DBPassword:         setupDBPass,
		AdminEmail:         setupAdminEmail,
		CloudSubjectID:     setupCloudSubjectID,
		PlatformDBHost:     os.Getenv("KORA_DB_HOST"),
		PlatformDBPort:     envIntDefault("KORA_DB_PORT", 0),
		PlatformDBUser:     os.Getenv("KORA_DB_USER"),
		PlatformDBPassword: os.Getenv("KORA_DB_PASSWORD"),
		PlatformDBDSN:      os.Getenv("DB_DSN"),
	}
	tenantConfig := createInput.ResolvedTenantConfig()
	platformDriver := createInput.PlatformDBType
	if platformDriver == "" {
		platformDriver = tenantConfig.DBType
	}
	result, err := runSetupSiteCreation(cmd.Context(), tenantConfig, platformDriver, createInput.PlatformDBDSN, func() (*site.CreateSiteResult, error) {
		return site.CreateSite(createInput)
	})
	if err != nil {
		return fmt.Errorf("creating site: %w", err)
	}
	defer result.DB.Close()

	fmt.Printf("  ✓ Site %s created (database: %s)\n", siteName, result.Config.DBName)
	fmt.Printf("  ✓ Admin user: %s\n", setupAdminEmail)

	// Import YAML config.
	slog.Info("importing config", "path", setupConfigPath)
	if err := runSetupConfigImport(cmd.Context(), result.Config, func() error {
		return site.ImportConfigForTenant(result.DB, result.Registry, result.Config.DBName, siteName, result.SiteID, setupConfigPath, kdb.Resolve(result.Config.DBType))
	}); err != nil {
		return fmt.Errorf("importing config: %w", err)
	}

	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────┐")
	fmt.Println("│            Kigae setup complete!                     │")
	fmt.Printf("│  Site:     %-40s │\n", siteName)
	fmt.Printf("│  Database: %-40s │\n", result.Config.DBName)
	fmt.Printf("│  Admin:    %-40s │\n", setupAdminEmail)
	fmt.Println("│                                                     │")
	fmt.Println("│  Start: kora serve                              │")
	fmt.Println("└─────────────────────────────────────────────────────┘")

	return nil
}

func runSetupConfigImport(ctx context.Context, config *site.SiteConfig, importConfig func() error) error {
	if ctx == nil || config == nil || importConfig == nil {
		return fmt.Errorf("setup config import requires context, tenant configuration, and operation")
	}
	release, err := acquireSiteCLIWork(ctx, config, true)
	if err != nil {
		return err
	}
	defer release()
	return importConfig()
}

func runSetupSiteCreation(ctx context.Context, tenantConfig *site.SiteConfig, platformDriver, platformDSN string, createSite func() (*site.CreateSiteResult, error)) (*site.CreateSiteResult, error) {
	if ctx == nil || tenantConfig == nil || createSite == nil {
		return nil, fmt.Errorf("setup site creation requires context, tenant configuration, and operation")
	}
	release, err := acquireSiteCreationWork(ctx, tenantConfig, platformDriver, platformDSN)
	if err != nil {
		return nil, err
	}
	defer release()
	return createSite()
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
