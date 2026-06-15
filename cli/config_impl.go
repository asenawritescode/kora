package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
	"github.com/spf13/cobra"
)

func init() {
	// Add config subcommands.
	configCmd.AddCommand(configExportCmd)
}

var (
	configExportSite string
	configExportPath string
)

func init() {
	configExportCmd.Flags().StringVar(&configExportSite, "site", "", "Target site hostname")
	configExportCmd.Flags().StringVar(&configExportPath, "path", "", "Output directory path")
}

var configExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export active config to YAML files",
	Long:  `Export the active config version from the database to YAML files.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigExport(configExportSite, configExportPath)
	},
}

func runConfigExport(siteName, path string) error {
	if siteName == "" || path == "" {
		return fmt.Errorf("--site and --path are required")
	}

	siteCfg, err := site.LoadSiteConfig(fmt.Sprintf("sites/%s/site_config.yaml", siteName))
	if err != nil {
		return fmt.Errorf("loading site config: %w", err)
	}

	db, err := site.Connect(siteCfg)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	store := configstore.NewStore(db)
	doctypes, err := store.LoadAll()
	if err != nil {
		return fmt.Errorf("loading doctypes: %w", err)
	}

	roles, err := store.LoadRoles()
	if err != nil {
		return fmt.Errorf("loading roles: %w", err)
	}
	permissions, err := store.LoadPermissions()
	if err != nil {
		return fmt.Errorf("loading permissions: %w", err)
	}
	workflows, err := store.LoadWorkflows()
	if err != nil {
		return fmt.Errorf("loading workflows: %w", err)
	}

	// Create output directory.
	os.MkdirAll(path, 0755)
	doctypesDir := path + "/doctypes"
	os.MkdirAll(doctypesDir, 0755)

	// Write each doctype as YAML.
	for _, dt := range doctypes {
		data, err := yaml.Marshal(dt)
		if err != nil {
			return fmt.Errorf("marshaling %s: %w", dt.Name, err)
		}
		filename := strings.ToLower(strings.ReplaceAll(dt.Name, " ", "_")) + ".yaml"
		if err := os.WriteFile(doctypesDir+"/"+filename, data, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", filename, err)
		}
		fmt.Printf("  ✓ %s\n", filename)
	}

	// Write roles.
	if len(roles) > 0 {
		data, _ := yaml.Marshal(roles)
		os.WriteFile(path+"/roles.yaml", data, 0644)
		fmt.Println("  ✓ roles.yaml")
	}

	// Write permissions.
	if len(permissions) > 0 {
		data, _ := yaml.Marshal(permissions)
		os.WriteFile(path+"/permissions.yaml", data, 0644)
		fmt.Println("  ✓ permissions.yaml")
	}

	// Write workflows.
	for _, wf := range workflows {
		data, _ := yaml.Marshal(wf)
		filename := strings.ToLower(strings.ReplaceAll(wf.Name, " ", "_")) + ".yaml"
		os.WriteFile(doctypesDir+"/"+filename, data, 0644)
		fmt.Printf("  ✓ %s (workflow)\n", filename)
	}

	fmt.Printf("\nExported %d doctypes, %d roles, %d permissions, %d workflows to %s\n",
		len(doctypes), len(roles), len(permissions), len(workflows), path)
	return nil
}

func runConfigImport(siteName, path string) error {
	// Load site config.
	siteCfg, err := site.LoadSiteConfig(fmt.Sprintf("sites/%s/site_config.yaml", siteName))
	if err != nil {
		return fmt.Errorf("loading site config: %w", err)
	}

	// Connect to database.
	db, err := site.Connect(siteCfg)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	// Bootstrap system tables if needed.
	if err := bootstrapSystemTables(db); err != nil {
		return fmt.Errorf("bootstrapping: %w", err)
	}

	// Parse DocType config files.
	doctypes, err := doctype.ParseConfigTree(path)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	fmt.Printf("Found %d DocTypes in %s\n", len(doctypes), path)

	// Parse roles.
	roles, permissions, err := doctype.ParseRolesDirectory(path)
	if err != nil {
		return fmt.Errorf("parsing roles: %w", err)
	}
	if len(roles) > 0 {
		fmt.Printf("Found %d roles\n", len(roles))
	}
	if len(permissions) > 0 {
		fmt.Printf("Found %d permissions\n", len(permissions))
	}

	// Parse workflows.
	workflows, err := doctype.ParseWorkflowDirectory(path)
	if err != nil {
		workflows = nil
	}
	if wf2, err := doctype.ParseWorkflowDirectory(path + "/doctypes"); err == nil {
		workflows = append(workflows, wf2...)
	}
	if len(workflows) > 0 {
		fmt.Printf("Found %d workflows\n", len(workflows))
	}

	// Save to database. Each SaveDocType is individually transactional.
	// Roles, permissions, and workflows are also individually saved.
	store := configstore.NewStore(db)
	for _, dt := range doctypes {
		if err := store.SaveDocType(dt); err != nil {
			return fmt.Errorf("saving %s: %w", dt.Name, err)
		}
		fmt.Printf("  ✓ %s (%d fields)\n", dt.Name, len(dt.Fields))
	}

	// Save roles and permissions.
	if err := store.SaveRoles(roles); err != nil {
		return fmt.Errorf("saving roles: %w", err)
	}
	if err := store.SavePermissions(permissions); err != nil {
		return fmt.Errorf("saving permissions: %w", err)
	}

	// Save workflows.
	if err := store.SaveWorkflows(workflows); err != nil {
		return fmt.Errorf("saving workflows: %w", err)
	}

	// Build registry with full config.
	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)

	// Load workflows into map.
	for _, wf := range workflows {
		registry.Workflows.Register(wf)
	}

	// Create config version BEFORE migration (so we have a snapshot to roll back to).
	// This is fatal — don't apply schema changes without a version record.
	versionID, versionNum, err := store.CreateConfigVersion(siteName, "system", "Config import from "+path, "Active", doctypes)
	if err != nil {
		return fmt.Errorf("creating config version: %w", err)
	}
	fmt.Printf("  ✓ Config version %d (%s) created\n", versionNum, versionID)

	// Run migration.
	if err := schema.MigrateSite(db, siteCfg.DBName, registry); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	// Print changelog summary.
		var changelogStr string
		db.QueryRow("SELECT COALESCE(changelog, '') FROM _kora_config_version WHERE id = ?", versionID).Scan(&changelogStr)
		if changelogStr != "" {
			var diff doctype.ConfigDiff
			if json.Unmarshal([]byte(changelogStr), &diff) == nil {
				if diff.IsBreaking {
					fmt.Printf("  ⚠️  Warning: %d breaking changes detected!\n", len(diff.BreakingChanges()))
					for _, c := range diff.BreakingChanges() {
						fmt.Printf("     - %s\n", c.Message)
					}
				}
				fmt.Printf("  ✓ %s\n", diff.Summary())
			}
		}

	fmt.Println("Config imported successfully.")
	return nil
}

func bootstrapSystemTables(db *sql.DB) error {
	systemTableSQL := []string{
		`CREATE TABLE IF NOT EXISTS _kora_doctype (
			name VARCHAR(140) PRIMARY KEY,
			module VARCHAR(140) NOT NULL DEFAULT '',
			is_submittable TINYINT(1) NOT NULL DEFAULT 0,
			is_child_table TINYINT(1) NOT NULL DEFAULT 0,
			is_single TINYINT(1) NOT NULL DEFAULT 0,
			track_changes TINYINT(1) NOT NULL DEFAULT 0,
			title_field VARCHAR(140) NOT NULL DEFAULT 'name',
			search_fields VARCHAR(255) NOT NULL DEFAULT '',
			sort_field VARCHAR(140) NOT NULL DEFAULT 'modified',
			sort_order VARCHAR(4) NOT NULL DEFAULT 'DESC',
			description TEXT,
			config_json JSON,
			version INT NOT NULL DEFAULT 1,
			creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// Add version column to existing _kora_doctype tables.
		`ALTER TABLE _kora_doctype ADD COLUMN version INT NOT NULL DEFAULT 1`,

		`CREATE TABLE IF NOT EXISTS _kora_field (
			name VARCHAR(140) PRIMARY KEY,
			parent VARCHAR(140) NOT NULL,
			fieldname VARCHAR(140) NOT NULL,
			fieldtype VARCHAR(50) NOT NULL,
			label VARCHAR(255) NOT NULL DEFAULT '',
			options TEXT,
			reqd TINYINT(1) NOT NULL DEFAULT 0,
			unique_constraint TINYINT(1) NOT NULL DEFAULT 0,
			default_value VARCHAR(255),
			hidden TINYINT(1) NOT NULL DEFAULT 0,
			read_only TINYINT(1) NOT NULL DEFAULT 0,
			bold TINYINT(1) NOT NULL DEFAULT 0,
			in_list_view TINYINT(1) NOT NULL DEFAULT 0,
			in_standard_filter TINYINT(1) NOT NULL DEFAULT 0,
			search_index TINYINT(1) NOT NULL DEFAULT 0,
			description TEXT,
			depends_on TEXT,
			mandatory_depends_on TEXT,
			constraints_json JSON,
			renamed_from VARCHAR(140),
			linked_field VARCHAR(255) NOT NULL DEFAULT '',
			computed TEXT,
			idx INT NOT NULL DEFAULT 0,
			INDEX idx_parent (parent),
			INDEX idx_parent_fieldname (parent, fieldname)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// Add linked_field column (ignore error if already exists).
	`ALTER TABLE _kora_field ADD COLUMN linked_field VARCHAR(255) NOT NULL DEFAULT ''`,
	`ALTER TABLE _kora_field ADD COLUMN computed TEXT`,

		`CREATE TABLE IF NOT EXISTS _kora_role (
			name VARCHAR(140) PRIMARY KEY,
			workspace_access TINYINT(1) NOT NULL DEFAULT 1,
			description TEXT
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_permission (
			name VARCHAR(140) PRIMARY KEY,
			doctype VARCHAR(140) NOT NULL,
			role VARCHAR(140) NOT NULL,
			can_read TINYINT(1) NOT NULL DEFAULT 0,
			can_write TINYINT(1) NOT NULL DEFAULT 0,
			can_create TINYINT(1) NOT NULL DEFAULT 0,
			can_delete TINYINT(1) NOT NULL DEFAULT 0,
			can_submit TINYINT(1) NOT NULL DEFAULT 0,
			can_cancel TINYINT(1) NOT NULL DEFAULT 0,
			can_amend TINYINT(1) NOT NULL DEFAULT 0,
			can_export TINYINT(1) NOT NULL DEFAULT 0,
			can_import TINYINT(1) NOT NULL DEFAULT 0,
			can_report TINYINT(1) NOT NULL DEFAULT 0,
			if_owner TINYINT(1) NOT NULL DEFAULT 0,
			UNIQUE KEY idx_doctype_role (doctype, role)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_config_version (
			id VARCHAR(36) PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			version INT NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			created_by VARCHAR(140) NOT NULL DEFAULT 'system',
			label VARCHAR(255) NOT NULL DEFAULT '',
			changelog JSON,
			status VARCHAR(20) NOT NULL DEFAULT 'Draft',
			config JSON,
			INDEX idx_site_status (site, status),
			INDEX idx_site_version (site, version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// Add status column (ignored if already exists). Also try adding is_active
		// for backwards compat, and migrate old data if both columns exist.
		`ALTER TABLE _kora_config_version ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'Superseded'`,
		`ALTER TABLE _kora_config_version ADD COLUMN is_active TINYINT(1) NOT NULL DEFAULT 0`,
		`UPDATE _kora_config_version SET status = 'Active' WHERE is_active = 1 AND status = 'Superseded'`,

		`CREATE TABLE IF NOT EXISTS _kora_user (
			name VARCHAR(140) PRIMARY KEY,
			email VARCHAR(255) NOT NULL DEFAULT '',
			password_hash VARCHAR(255) NOT NULL,
			full_name VARCHAR(255) NOT NULL DEFAULT '',
			enabled TINYINT(1) NOT NULL DEFAULT 1,
			roles TEXT,
			creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			UNIQUE KEY idx_email (email)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_session (
			sid VARCHAR(255) PRIMARY KEY,
			user VARCHAR(140) NOT NULL,
			data JSON,
			expires_at DATETIME(6) NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			INDEX idx_user (user),
			INDEX idx_expires (expires_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}

	for _, ddl := range systemTableSQL {
		if _, err := db.Exec(ddl); err != nil {
			// Ignore duplicate column errors and unknown column errors
			// (for idempotent ALTER TABLE and migration UPDATE statements).
			errStr := err.Error()
			if strings.Contains(errStr, "Duplicate column") ||
				strings.Contains(errStr, "Unknown column") {
				continue
			}
			return fmt.Errorf("creating system table: %w\nSQL: %s", err, ddl)
		}
	}
	return nil
}

// --- Config versioning CLI subcommands ---

var configVersionsSite string
var configDiffSite, configDiffFrom, configDiffTo string
var configRollbackSite string
var configRollbackToVersion int

func init() {
	versionsCmd := &cobra.Command{
		Use:   "versions",
		Short: "List config version history",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configVersionsSite == "" {
				return fmt.Errorf("--site is required")
			}
			return runConfigVersions(configVersionsSite)
		},
	}
	versionsCmd.Flags().StringVar(&configVersionsSite, "site", "", "Site hostname")
	configCmd.AddCommand(versionsCmd)

	diffCmd := &cobra.Command{
		Use:   "diff",
		Short: "Diff two config versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigDiff(configDiffSite, configDiffFrom, configDiffTo)
		},
	}
	diffCmd.Flags().StringVar(&configDiffSite, "site", "", "Site hostname")
	diffCmd.Flags().StringVar(&configDiffFrom, "from", "", "From version ID")
	diffCmd.Flags().StringVar(&configDiffTo, "to", "", "To version ID")
	configCmd.AddCommand(diffCmd)

	rollbackCmd := &cobra.Command{
		Use:   "rollback",
		Short: "Rollback to a previous config version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigRollback(configRollbackSite, configRollbackToVersion)
		},
	}
	rollbackCmd.Flags().StringVar(&configRollbackSite, "site", "", "Site hostname")
	rollbackCmd.Flags().IntVar(&configRollbackToVersion, "to-version", 0, "Target version number")
	configCmd.AddCommand(rollbackCmd)
}

func runConfigVersions(siteName string) error {
	siteCfg, err := site.LoadSiteConfig(fmt.Sprintf("sites/%s/site_config.yaml", siteName))
	if err != nil {
		return err
	}
	db, err := site.Connect(siteCfg)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Query(
		"SELECT version, created_at, created_by, label, is_active FROM _kora_config_version WHERE site = ? ORDER BY version DESC",
		siteName,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Printf("%-8s %-20s %-15s %s\n", "VERSION", "CREATED", "BY", "LABEL")
	fmt.Println(strings.Repeat("-", 80))
	for rows.Next() {
		var version int
		var createdAt, createdBy, label string
		var isActive bool
		rows.Scan(&version, &createdAt, &createdBy, &label, &isActive)
		active := ""
		if isActive {
			active = " (active)"
		}
		fmt.Printf("%-8d %-20s %-15s %s%s\n", version, createdAt[:min19(createdAt)], createdBy, label, active)
	}
	return nil
}

func min19(s string) int {
	if len(s) > 19 {
		return 19
	}
	return len(s)
}

func runConfigDiff(siteName, fromID, toID string) error {
	if fromID == "" || toID == "" {
		return fmt.Errorf("--from and --to are required")
	}
	siteCfg, _ := site.LoadSiteConfig(fmt.Sprintf("sites/%s/site_config.yaml", siteName))
	db, _ := site.Connect(siteCfg)
	defer db.Close()

	var fromJSON, toJSON string
	db.QueryRow("SELECT config FROM _kora_config_version WHERE id = ?", fromID).Scan(&fromJSON)
	db.QueryRow("SELECT config FROM _kora_config_version WHERE id = ?", toID).Scan(&toJSON)

	var from, to []*doctype.DocType
	yaml.Unmarshal([]byte(fromJSON), &from)
	yaml.Unmarshal([]byte(toJSON), &to)

	diff := doctype.DiffConfigs(from, to)
	fmt.Printf("Changes from version %s to %s: %s\n", fromID, toID, diff.Summary())
	for _, c := range diff.Changes {
		flag := " "
		if c.Breaking {
			flag = "⚠"
		}
		fmt.Printf("  %s %s\n", flag, c.Message)
	}
	return nil
}

func runConfigRollback(siteName string, toVersion int) error {
	if toVersion < 1 {
		return fmt.Errorf("--to-version must be >= 1")
	}
	siteCfg, err := site.LoadSiteConfig(fmt.Sprintf("sites/%s/site_config.yaml", siteName))
	if err != nil {
		return err
	}
	db, err := site.Connect(siteCfg)
	if err != nil {
		return err
	}
	defer db.Close()

	var targetJSON string
	err = db.QueryRow(
		"SELECT config FROM _kora_config_version WHERE site = ? AND version = ?",
		siteName, toVersion,
	).Scan(&targetJSON)
	if err != nil {
		return fmt.Errorf("version %d not found: %w", toVersion, err)
	}

	var targetDocTypes []*doctype.DocType
	if err := yaml.Unmarshal([]byte(targetJSON), &targetDocTypes); err != nil {
		return fmt.Errorf("parsing version %d: %w", toVersion, err)
	}

	fmt.Printf("Rolling back to version %d (%d doctypes)...\n", toVersion, len(targetDocTypes))
	db.Exec("UPDATE _kora_config_version SET is_active = 0 WHERE site = ?", siteName)
	db.Exec("UPDATE _kora_config_version SET is_active = 1 WHERE site = ? AND version = ?", siteName, toVersion)
	fmt.Println("Rollback complete.")
	return nil
}
