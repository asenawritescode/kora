package site

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/configstore"
	sqlDialect "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
)

// ImportConfig parses YAML config from a directory, saves it to the database,
// builds the registry, and runs schema migration. This is a separate composable
// step called only when a YAML config directory is provided (CLI setup path).
// Console-created sites skip this — users configure doctypes via AI or admin UI.
func ImportConfig(db *sql.DB, registry *doctype.Registry, dbName, siteName, configPath string, dialect sqlDialect.Dialect) error {
	// Step 1: Parse DocType config files.
	doctypes, err := doctype.ParseConfigTree(configPath)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// Step 2: Parse roles and permissions.
	roles, permissions, err := doctype.ParseRolesDirectory(configPath)
	if err != nil {
		return fmt.Errorf("parsing roles: %w", err)
	}

	// Step 3: Parse workflows (from both root and doctypes subdirectory).
	workflows, _ := doctype.ParseWorkflowDirectory(configPath)
	if wf2, err := doctype.ParseWorkflowDirectory(configPath + "/doctypes"); err == nil {
		workflows = append(workflows, wf2...)
	}

	// Step 4: Save to database.
	store := configstore.NewStore(db)
	for _, dt := range doctypes {
		if err := store.SaveDocType(dt); err != nil {
			return fmt.Errorf("saving doctype %s: %w", dt.Name, err)
		}
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

	// Step 5: Build registry with full config.
	registry.LoadFull(doctypes, roles, permissions)
	for _, wf := range workflows {
		registry.Workflows.Register(wf)
	}

	// Step 6: Create config version BEFORE migration (so we have a rollback snapshot).
	versionID, _, err := store.CreateConfigVersion(siteName, "system", "Config import from "+configPath, "Active", doctypes)
	if err != nil {
		return fmt.Errorf("creating config version: %w", err)
	}

	// Step 7: Run schema migration.
	if err := schema.MigrateSite(db, dbName, registry, dialect); err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}

	// Print changelog summary.
	var changelogStr string
	db.QueryRow("SELECT COALESCE(changelog, '') FROM _kora_config_version WHERE id = ?", versionID).Scan(&changelogStr)
	if changelogStr != "" {
		var diff doctype.ConfigDiff
		if json.Unmarshal([]byte(changelogStr), &diff) == nil {
			if diff.IsBreaking {
				fmt.Printf("  ⚠️  Warning: %d breaking changes!\n", len(diff.BreakingChanges()))
				for _, c := range diff.BreakingChanges() {
					fmt.Printf("     - %s\n", c.Message)
				}
			}
			fmt.Printf("  ✓ %s\n", diff.Summary())
		}
	}

	return nil
}
