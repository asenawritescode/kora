package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
	"github.com/yourorg/kora/auth"
	"github.com/yourorg/kora/configstore"
	"github.com/yourorg/kora/doctype"
	"github.com/yourorg/kora/schema"
	"github.com/yourorg/kora/site"
)

var (
	setupDBHost       string
	setupDBPort       int
	setupDBUser       string
	setupDBPass       string
	setupDBName       string
	setupAdminEmail   string
	setupAdminPass    string
	setupConfigPath   string
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

	setupCmd.Flags().StringVar(&setupDBHost, "db-host", "127.0.0.1", "MySQL host")
	setupCmd.Flags().IntVar(&setupDBPort, "db-port", 3306, "MySQL port")
	setupCmd.Flags().StringVar(&setupDBUser, "db-user", "root", "MySQL user")
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

	if setupDBName == "" {
		setupDBName = hostnameToDBName(siteName)
	}

	slog.Info("starting setup", "site", siteName, "db", setupDBName, "config", setupConfigPath)

	// Step 1: Create database.
	slog.Info("creating database", "db", setupDBName)
	siteCfg := &site.SiteConfig{
		DBHost:     setupDBHost,
		DBPort:     setupDBPort,
		DBUser:     setupDBUser,
		DBPassword: setupDBPass,
		DBName:     setupDBName,
		Hostname:   siteName,
		FileStorage: "local",
		FilesPath:   fmt.Sprintf("sites/%s/files", siteName),
		Apps:        []string{"core"},
	}

	if err := site.CreateDatabase(siteCfg); err != nil {
		return fmt.Errorf("creating database: %w", err)
	}
	fmt.Printf("  ✓ Database %s created\n", setupDBName)

	// Step 2: Write site config.
	if err := writeSiteConfig(siteName, siteCfg); err != nil {
		return fmt.Errorf("writing site config: %w", err)
	}
	fmt.Printf("  ✓ Site config written to sites/%s/site_config.yaml\n", siteName)

	// Step 3: Connect to the new database.
	db, err := site.Connect(siteCfg)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	// Step 4: Bootstrap system tables.
	slog.Info("bootstrapping system tables")
	if err := bootstrapSystemTables(db); err != nil {
		return fmt.Errorf("bootstrapping system tables: %w", err)
	}
	fmt.Println("  ✓ System tables bootstrapped")

	// Step 5: Parse and import config.
	slog.Info("parsing config", "path", setupConfigPath)
	doctypes, err := doctype.ParseConfigTree(setupConfigPath)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	fmt.Printf("  ✓ Found %d DocTypes\n", len(doctypes))

	roles, permissions, err := doctype.ParseRolesDirectory(setupConfigPath)
	if err != nil {
		return fmt.Errorf("parsing roles: %w", err)
	}
	if len(roles) > 0 {
		fmt.Printf("  ✓ Found %d roles\n", len(roles))
	}
	if len(permissions) > 0 {
		fmt.Printf("  ✓ Found %d permissions\n", len(permissions))
	}

	workflows, err := doctype.ParseWorkflowDirectory(setupConfigPath)
	if err != nil {
		workflows = nil
	}
	// Also check doctypes subdirectory.
	if wf2, err := doctype.ParseWorkflowDirectory(setupConfigPath + "/doctypes"); err == nil {
		workflows = append(workflows, wf2...)
	}
	if len(workflows) > 0 {
		fmt.Printf("  ✓ Found %d workflows\n", len(workflows))
	}

	// Step 6: Save to database.
	store := configstore.NewStore(db)
	for _, dt := range doctypes {
		if err := store.SaveDocType(dt); err != nil {
			return fmt.Errorf("saving doctype %s: %w", dt.Name, err)
		}
		fmt.Printf("  ✓ %s (%d fields)\n", dt.Name, len(dt.Fields))
	}

	if err := store.SaveRoles(roles); err != nil {
		return fmt.Errorf("saving roles: %w", err)
	}
	if err := store.SavePermissions(permissions); err != nil {
		return fmt.Errorf("saving permissions: %w", err)
	}
	if err := store.SaveWorkflows(workflows); err != nil {
		return fmt.Errorf("saving workflows: %w", err)
	}

	// Step 7: Build registry and migrate.
	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)
	for _, wf := range workflows {
		registry.Workflows.Register(wf)
	}

	slog.Info("running schema migration")
	if err := schema.MigrateSite(db, setupDBName, registry); err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}
	fmt.Println("  ✓ Schema migrated")

	// Step 8: Create admin user.
	slog.Info("creating admin user", "email", setupAdminEmail)
	passwordHash, err := auth.HashPassword(setupAdminPass)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	_, err = db.Exec(
		`INSERT INTO _kora_user (name, email, password_hash, full_name, roles)
		 VALUES (?, ?, ?, ?, ?)`,
		"Administrator", setupAdminEmail, passwordHash, "Administrator", "Administrator",
	)
	if err != nil {
		return fmt.Errorf("creating admin user: %w", err)
	}
	fmt.Printf("  ✓ Admin user created: %s\n", setupAdminEmail)

	// Step 9: Create initial config version.
	versionID := ulid.Make().String()
	_, err = db.Exec(
		`INSERT INTO _kora_config_version (id, site, version, created_by, label, is_active, config)
		 VALUES (?, ?, 1, 'setup', 'Initial setup', 1, '{}')`,
		versionID, siteName,
	)
	if err != nil {
		slog.Warn("failed to create initial config version", "error", err)
	} else {
		fmt.Println("  ✓ Initial config version created")
	}

	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────┐")
	fmt.Println("│            Kora setup complete!                     │")
	fmt.Printf("│  Site:     %-40s │\n", siteName)
	fmt.Printf("│  Database: %-40s │\n", setupDBName)
	fmt.Printf("│  Admin:    %-40s │\n", setupAdminEmail)
	fmt.Println("│                                                     │")
	fmt.Printf("│  Start: kora serve --site %-25s │\n", siteName)
	fmt.Println("└─────────────────────────────────────────────────────┘")

	return nil
}

func writeSiteConfig(hostname string, cfg *site.SiteConfig) error {
	siteDir := fmt.Sprintf("sites/%s", hostname)
	if err := os.MkdirAll(siteDir, 0755); err != nil {
		return err
	}
	filesDir := fmt.Sprintf("%s/files", siteDir)
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		return err
	}

	content := fmt.Sprintf(`# Site configuration for %s
db_host: %s
db_port: %d
db_name: %s
db_user: %s
db_password: %s

redis_url: redis://localhost:6379/0

file_storage: local
files_path: sites/%s/files

apps:
  - core

hostname: %s
`, hostname, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, cfg.DBPassword, hostname, hostname)

	return os.WriteFile(fmt.Sprintf("%s/site_config.yaml", siteDir), []byte(content), 0644)
}
