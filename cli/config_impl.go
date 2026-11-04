package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/dbaccess"
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
		return runConfigExportContext(cmd.Context(), configExportSite, configExportPath)
	},
}

func runConfigExport(siteName, path string) error {
	return runConfigExportContext(context.Background(), siteName, path)
}

func runConfigExportContext(ctx context.Context, siteName, path string) error {
	if siteName == "" || path == "" {
		return fmt.Errorf("--site and --path are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	common_cfg := site.CommonConfigFromEnv()
	siteCfg := site.ReconstructSiteConfig(siteName, common_cfg, nil)

	release, err := acquireSiteCLIWork(ctx, siteCfg, false)
	if err != nil {
		return err
	}
	db, err := site.ConnectContext(ctx, siteCfg)
	if err != nil {
		release()
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()
	defer release()

	store := configstore.NewStore(db, kdb.Resolve(siteCfg.DBType))
	doctypes, err := store.LoadAll(siteName)
	if err != nil {
		return fmt.Errorf("loading doctypes: %w", err)
	}

	roles, err := store.LoadRoles(siteName)
	if err != nil {
		return fmt.Errorf("loading roles: %w", err)
	}
	permissions, err := store.LoadPermissions(siteName)
	if err != nil {
		return fmt.Errorf("loading permissions: %w", err)
	}
	views, err := store.LoadViews(siteName)
	if err != nil {
		return fmt.Errorf("loading views: %w", err)
	}

	files := make(map[string][]byte)
	for _, dt := range doctypes {
		data, err := yaml.Marshal(dt)
		if err != nil {
			return fmt.Errorf("marshaling %s: %w", dt.Name, err)
		}
		name, err := configExportFilename("doctypes", dt.Name)
		if err != nil {
			return fmt.Errorf("naming doctype %q export: %w", dt.Name, err)
		}
		if err := addConfigExportFile(files, name, data); err != nil {
			return fmt.Errorf("doctype %q export: %w", dt.Name, err)
		}
	}
	if len(roles) > 0 {
		data, err := yaml.Marshal(roles)
		if err != nil {
			return fmt.Errorf("marshaling roles: %w", err)
		}
		if err := addConfigExportFile(files, "roles.yaml", data); err != nil {
			return err
		}
	}
	if len(permissions) > 0 {
		data, err := yaml.Marshal(permissions)
		if err != nil {
			return fmt.Errorf("marshaling permissions: %w", err)
		}
		if err := addConfigExportFile(files, "permissions.yaml", data); err != nil {
			return err
		}
	}
	for _, view := range views {
		if view != nil && view.PageManifest != nil {
			data, err := marshalPageManifestYAML(view.PageManifest)
			if err != nil {
				return fmt.Errorf("marshaling page manifest %s: %w", view.Name, err)
			}
			name, err := configExportFilename("pages", view.PageManifest.Metadata.Name)
			if err != nil {
				return fmt.Errorf("naming page manifest %q export: %w", view.Name, err)
			}
			if err := addConfigExportFile(files, name, data); err != nil {
				return fmt.Errorf("page manifest %q export: %w", view.Name, err)
			}
			continue
		}
		data, err := yaml.Marshal(view)
		if err != nil {
			return fmt.Errorf("marshaling view %s: %w", view.Name, err)
		}
		name, err := configExportFilename("views", view.Name)
		if err != nil {
			return fmt.Errorf("naming view %q export: %w", view.Name, err)
		}
		if err := addConfigExportFile(files, name, data); err != nil {
			return fmt.Errorf("view %q export: %w", view.Name, err)
		}
	}
	if err := publishConfigExport(path, files); err != nil {
		return fmt.Errorf("publishing config export: %w", err)
	}
	for name := range files {
		fmt.Printf("  ✓ %s\n", name)
	}

	fmt.Printf("\nExported %d doctypes, %d roles, %d permissions, %d views to %s\n",
		len(doctypes), len(roles), len(permissions), len(views), path)
	return nil
}

func marshalPageManifestYAML(manifest *doctype.PageManifest) ([]byte, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, err
	}
	return yaml.Marshal(document)
}

func runConfigImport(siteName, path, dbName string) (returnErr error) {
	return runConfigImportContext(context.Background(), siteName, path, dbName)
}

func runConfigImportContext(ctx context.Context, siteName, path, dbName string) (returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Load site config.
	common_cfg := site.CommonConfigFromEnv()
	siteCfg := resolveConfigImportSiteConfigContext(ctx, siteName, dbName, common_cfg)
	fmt.Printf("Import target: site=%s database=%s\n", siteName, siteCfg.DBName)

	// Connect to database.
	release, err := acquireSiteCLIWork(ctx, siteCfg, true)
	if err != nil {
		return err
	}
	db, err := site.ConnectContext(ctx, siteCfg)
	if err != nil {
		release()
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()
	defer release()

	// Bootstrap system tables if needed.
	if err := site.BootstrapSystemTables(db, kdb.Resolve(siteCfg.DBType)); err != nil {
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

	// Parse the entire config pack before the first tenant-config write. A bad
	// optional file must not leave an import half-applied.
	views, err := doctype.ParseViewsDirectory(path + "/views")
	if err != nil {
		return fmt.Errorf("parsing views: %w", err)
	}
	pageManifests, err := doctype.ParsePageManifestsDirectory(path + "/pages")
	if err != nil {
		return fmt.Errorf("parsing page manifests: %w", err)
	}
	viewNames := make(map[string]struct{}, len(views))
	for _, view := range views {
		if view != nil {
			viewNames[strings.ToLower(strings.TrimSpace(view.Name))] = struct{}{}
		}
	}
	for _, manifest := range pageManifests {
		key := strings.ToLower(strings.TrimSpace(manifest.Metadata.Name))
		if _, exists := viewNames[key]; exists {
			return fmt.Errorf("page manifest %q duplicates a view name; define it in pages/ or views/, not both", manifest.Metadata.Name)
		}
		viewNames[key] = struct{}{}
		views = append(views, manifest.ToView())
	}
	if len(views) > 0 {
		fmt.Printf("Found %d views\n", len(views))
	}
	reportDefinitions, err := analytics.ParseReportsDirectory(path + "/reports")
	if err != nil {
		return fmt.Errorf("parsing reports: %w", err)
	}
	reports := make([]json.RawMessage, 0, len(reportDefinitions))
	for _, report := range reportDefinitions {
		data, err := json.Marshal(report)
		if err != nil {
			return fmt.Errorf("encoding report %s: %w", report.Name, err)
		}
		reports = append(reports, data)
	}
	if len(reports) > 0 {
		fmt.Printf("Found %d reports\n", len(reports))
	}

	snapshot := &doctype.ConfigSnapshot{DocTypes: doctypes, Roles: roles, Permissions: permissions, Views: views, Reports: reports, MinKoraVersion: Version}
	snapshotBytes, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encoding config import snapshot: %w", err)
	}
	dialect := kdb.Resolve(siteCfg.DBType)
	store := configstore.NewStore(db, dialect)
	liveDocTypes, err := store.LoadAll(siteName)
	if err != nil {
		return fmt.Errorf("loading existing DocTypes for reference validation: %w", err)
	}
	liveRegistry := doctype.NewRegistry()
	liveRegistry.LoadFull(liveDocTypes, nil, nil)
	if findings := doctype.ValidateSnapshotReferences(snapshot, liveRegistry); len(findings) > 0 {
		return fmt.Errorf("configuration has unresolved references: %s at %s", findings[0].Code, findings[0].Path)
	}
	operationID := configImportOperationID(siteName, snapshotBytes)
	operation, err := configstore.EnsureActivationOperation(db, dialect, siteName, operationID, "import", snapshot)
	if err != nil {
		return fmt.Errorf("recording config import operation: %w", err)
	}
	if operation.Phase == configstore.ActivationActivated {
		return fmt.Errorf("config import operation %s has an invalid activated phase", operationID)
	}
	if operation.Phase != configstore.ActivationCompleted && operation.Phase != configstore.ActivationValidated {
		if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "import", configstore.ActivationApplying, operation.DDLPlan, "resume-from-live-schema", ""); err != nil {
			return fmt.Errorf("starting config import operation: %w", err)
		}
		operation.Phase = configstore.ActivationApplying
	}
	journalPhase := operation.Phase
	defer func() {
		if returnErr == nil || journalPhase == configstore.ActivationCompleted {
			return
		}
		if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "import", journalPhase, operation.DDLPlan, operation.RecoveryPlan, returnErr.Error()); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("recording config import failure: %w", err))
		}
	}()

	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)
	versionID := strings.TrimSpace(operation.RecoveryPlan)
	if operation.Phase != configstore.ActivationValidated && operation.Phase != configstore.ActivationCompleted {
		// Save to database. These idempotent upserts may be interrupted; retries
		// reuse the immutable journal snapshot and repeat them before version commit.
		for _, dt := range doctypes {
			if err := store.SaveDocType(dt, siteName); err != nil {
				return fmt.Errorf("saving %s: %w", dt.Name, err)
			}
			fmt.Printf("  ✓ %s (%d fields)\n", dt.Name, len(dt.Fields))
		}

		// Save roles and permissions as one authorization snapshot.
		authTx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("begin authorization config import: %w", err)
		}
		defer authTx.Rollback()
		if err := store.SaveRolesTx(authTx, roles, siteName); err != nil {
			return fmt.Errorf("saving roles: %w", err)
		}
		if err := store.SavePermissionsTx(authTx, permissions, siteName); err != nil {
			return fmt.Errorf("saving permissions: %w", err)
		}
		if err := configstore.BumpAuthorizationRevisionTx(authTx, kdb.Resolve(siteCfg.DBType), true, false); err != nil {
			return fmt.Errorf("advancing policy revision: %w", err)
		}
		if err := authTx.Commit(); err != nil {
			return fmt.Errorf("committing authorization config import: %w", err)
		}

		// Save views from the dedicated views directory only. Root-level YAML files
		// are reserved for doctypes, roles, and permissions.
		if len(views) > 0 {
			if err := store.SaveViews(views, siteName); err != nil {
				return fmt.Errorf("saving views: %w", err)
			}
			for _, v := range views {
				fmt.Printf("  ✓ %s (view)\n", v.Name)
			}
		}

		// Version history and the journal phase commit together so a lost CLI
		// response cannot create duplicate versions on retry.
		versionTx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("beginning config import version transaction: %w", err)
		}
		defer versionTx.Rollback()
		var versionNum int
		versionID, versionNum, err = store.CreateConfigVersionTx(versionTx, siteName, "system", "Config import from "+path, "Active", snapshot, "")
		if err != nil {
			return fmt.Errorf("creating config version: %w", err)
		}
		if err := configstore.UpdateActivationOperationPhaseTx(versionTx, dialect, siteName, operationID, "import", configstore.ActivationApplying, configstore.ActivationValidated, versionID); err != nil {
			return err
		}
		if err := versionTx.Commit(); err != nil {
			return fmt.Errorf("committing config import version: %w", err)
		}
		journalPhase = configstore.ActivationValidated
		operation.RecoveryPlan = versionID
		fmt.Printf("  ✓ Config version %d (%s) created\n", versionNum, versionID)
	} else if versionID == "" {
		return fmt.Errorf("config import operation %s is missing its committed version reference", operationID)
	}

	// Run migration.
	if err := schema.MigrateSiteFromRegistry(db, siteCfg.DBName, registry, kdb.Resolve(siteCfg.DBType)); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "import", configstore.ActivationCompleted, operation.DDLPlan, versionID, ""); err != nil {
		return fmt.Errorf("finalizing config import operation: %w", err)
	}
	journalPhase = configstore.ActivationCompleted
	// Print changelog summary.
	var changelogStr string
	if err := db.QueryRow("SELECT COALESCE(changelog, '') FROM _kora_config_version WHERE id = ?", versionID).Scan(&changelogStr); err != nil {
		return fmt.Errorf("reading imported config changelog: %w", err)
	}
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

func configImportOperationID(siteName string, snapshot []byte) string {
	input := make([]byte, 0, len(siteName)+1+len(snapshot))
	input = append(input, siteName...)
	input = append(input, 0)
	input = append(input, snapshot...)
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:18])
}

func resolveConfigImportSiteConfig(siteName, dbName string, commonCfg *site.CommonConfig) *site.SiteConfig {
	return resolveConfigImportSiteConfigContext(context.Background(), siteName, dbName, commonCfg)
}

func resolveConfigImportSiteConfigContext(ctx context.Context, siteName, dbName string, commonCfg *site.CommonConfig) *site.SiteConfig {
	if info, ok := lookupRegisteredSiteContext(ctx, siteName, commonCfg); ok {
		cfg := site.ReconstructSiteConfigFromDBInfo(info, commonCfg)
		if dbName != "" {
			cfg.DBName = dbName
		}
		return cfg
	}

	cfg := site.ReconstructSiteConfig(siteName, commonCfg, nil)
	if dbName != "" {
		cfg.DBName = dbName
	}
	return cfg
}

func lookupRegisteredSite(siteName string, commonCfg *site.CommonConfig) (site.DBSiteInfo, bool) {
	return lookupRegisteredSiteContext(context.Background(), siteName, commonCfg)
}

func lookupRegisteredSiteContext(ctx context.Context, siteName string, commonCfg *site.CommonConfig) (site.DBSiteInfo, bool) {
	db, err := openSiteDiscoveryDBContext(ctx, commonCfg)
	if err != nil {
		return site.DBSiteInfo{}, false
	}
	defer db.Close()

	var sites []site.DBSiteInfo
	var release func()
	if dsn := os.Getenv("DB_DSN"); dsn != "" {
		driver := commonCfg.DBType
		if driver == "" {
			driver = os.Getenv("KORA_DB_TYPE")
		}
		if driver == "" {
			driver = "mysql"
		}
		release, err = acquireCLIWork(ctx, driver, dsn, false)
	} else {
		config := site.ReconstructSiteConfig("_discovery_", commonCfg, nil)
		release, err = acquireSiteCLIWork(ctx, config, false)
	}
	if err != nil {
		return site.DBSiteInfo{}, false
	}
	err = func() error {
		defer release()
		var discoverErr error
		sites, discoverErr = site.DiscoverSitesFromDBContext(ctx, db)
		return discoverErr
	}()
	if err != nil {
		return site.DBSiteInfo{}, false
	}
	for _, info := range sites {
		if info.Name == siteName {
			return info, true
		}
		for _, domain := range info.Domains {
			if domain == siteName {
				return info, true
			}
		}
	}
	return site.DBSiteInfo{}, false
}

func openSiteDiscoveryDB(commonCfg *site.CommonConfig) (*sql.DB, error) {
	return openSiteDiscoveryDBContext(context.Background(), commonCfg)
}

func openSiteDiscoveryDBContext(ctx context.Context, commonCfg *site.CommonConfig) (*sql.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dsn := os.Getenv("DB_DSN"); dsn != "" {
		driver := commonCfg.DBType
		if driver == "" {
			driver = os.Getenv("KORA_DB_TYPE")
		}
		if driver == "" {
			driver = "mysql"
		}
		db, err := dbaccess.Open(driver, dsn)
		if err != nil {
			return nil, err
		}
		release, err := acquireCLIWork(ctx, driver, dsn, false)
		if err != nil {
			db.Close()
			return nil, err
		}
		pingErr := db.PingContext(ctx)
		release()
		if pingErr != nil {
			db.Close()
			return nil, pingErr
		}
		return db, nil
	}

	discoveryCfg := site.ReconstructSiteConfig("_discovery_", commonCfg, nil)
	release, err := acquireSiteCLIWork(ctx, discoveryCfg, false)
	if err != nil {
		return nil, err
	}
	db, err := site.ConnectContext(ctx, discoveryCfg)
	release()
	return db, err
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
			return runConfigVersionsContext(cmd.Context(), configVersionsSite)
		},
	}
	versionsCmd.Flags().StringVar(&configVersionsSite, "site", "", "Site hostname")
	configCmd.AddCommand(versionsCmd)

	diffCmd := &cobra.Command{
		Use:   "diff",
		Short: "Diff two config versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigDiffContext(cmd.Context(), configDiffSite, configDiffFrom, configDiffTo)
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
			return runConfigRollbackContext(cmd.Context(), configRollbackSite, configRollbackToVersion)
		},
	}
	rollbackCmd.Flags().StringVar(&configRollbackSite, "site", "", "Site hostname")
	rollbackCmd.Flags().IntVar(&configRollbackToVersion, "to-version", 0, "Target version number")
	configCmd.AddCommand(rollbackCmd)
}

func runConfigVersions(siteName string) error {
	return runConfigVersionsContext(context.Background(), siteName)
}

func runConfigVersionsContext(ctx context.Context, siteName string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	common_cfg := site.CommonConfigFromEnv()
	siteCfg := site.ReconstructSiteConfig(siteName, common_cfg, nil)
	release, err := acquireSiteCLIWork(ctx, siteCfg, false)
	if err != nil {
		return err
	}
	db, err := site.ConnectContext(ctx, siteCfg)
	if err != nil {
		release()
		return err
	}
	defer db.Close()
	defer release()

	rows, err := db.QueryContext(ctx,
		kdb.Rebind(kdb.Resolve(siteCfg.DBType), "SELECT version, created_at, created_by, label, status FROM _kora_config_version WHERE site = ? ORDER BY version DESC"),
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
		var status string
		if err := rows.Scan(&version, &createdAt, &createdBy, &label, &status); err != nil {
			return fmt.Errorf("reading config version row: %w", err)
		}
		active := ""
		if status == "Active" {
			active = " (active)"
		} else if status == "Draft" {
			active = " (draft)"
		}
		fmt.Printf("%-8d %-20s %-15s %s%s\n", version, createdAt[:min19(createdAt)], createdBy, label, active)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating config versions: %w", err)
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
	return runConfigDiffContext(context.Background(), siteName, fromID, toID)
}

func runConfigDiffContext(ctx context.Context, siteName, fromID, toID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if fromID == "" || toID == "" {
		return fmt.Errorf("--from and --to are required")
	}
	common_cfg := site.CommonConfigFromEnv()
	siteCfg := site.ReconstructSiteConfig(siteName, common_cfg, nil)
	release, err := acquireSiteCLIWork(ctx, siteCfg, false)
	if err != nil {
		return err
	}
	db, err := site.ConnectContext(ctx, siteCfg)
	if err != nil {
		release()
		return fmt.Errorf("connecting to site database: %w", err)
	}
	defer db.Close()
	defer release()

	var fromJSON, toJSON string
	dialect := kdb.Resolve(siteCfg.DBType)
	if err := db.QueryRowContext(ctx, kdb.Rebind(dialect, "SELECT config FROM _kora_config_version WHERE id = ?"), fromID).Scan(&fromJSON); err != nil {
		return fmt.Errorf("reading source config version %q: %w", fromID, err)
	}
	if err := db.QueryRowContext(ctx, kdb.Rebind(dialect, "SELECT config FROM _kora_config_version WHERE id = ?"), toID).Scan(&toJSON); err != nil {
		return fmt.Errorf("reading target config version %q: %w", toID, err)
	}

	fromSnapshot, err := doctype.ParseConfig(fromJSON)
	if err != nil {
		return fmt.Errorf("parsing source config version %q: %w", fromID, err)
	}
	toSnapshot, err := doctype.ParseConfig(toJSON)
	if err != nil {
		return fmt.Errorf("parsing target config version %q: %w", toID, err)
	}

	diff := doctype.DiffConfigs(fromSnapshot.DocTypes, toSnapshot.DocTypes)
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
	return runConfigRollbackContext(context.Background(), siteName, toVersion)
}

func runConfigRollbackContext(ctx context.Context, siteName string, toVersion int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if toVersion < 1 {
		return fmt.Errorf("--to-version must be >= 1")
	}
	common_cfg := site.CommonConfigFromEnv()
	siteCfg := site.ReconstructSiteConfig(siteName, common_cfg, nil)
	release, err := acquireSiteCLIWork(ctx, siteCfg, true)
	if err != nil {
		return err
	}
	db, err := site.ConnectContext(ctx, siteCfg)
	if err != nil {
		release()
		return err
	}
	defer db.Close()
	defer release()

	var targetJSON string
	var targetVersionID string
	dialect := kdb.Resolve(siteCfg.DBType)
	err = db.QueryRowContext(ctx, kdb.Rebind(dialect,
		"SELECT id, config FROM _kora_config_version WHERE site = ? AND version = ?"),
		siteName, toVersion,
	).Scan(&targetVersionID, &targetJSON)
	if err != nil {
		return fmt.Errorf("version %d not found: %w", toVersion, err)
	}

	// Parse the version snapshot with backward compatibility.
	snapshot, err := doctype.ParseConfig(targetJSON)
	if err != nil {
		return fmt.Errorf("parsing version %d: %w", toVersion, err)
	}

	fmt.Printf("Rolling back to version %d (%d doctypes)...\n", toVersion, len(snapshot.DocTypes))

	store := configstore.NewStore(db, dialect)

	// Build a temporary registry and run migration.
	reg := doctype.NewRegistry()
	for _, dt := range snapshot.DocTypes {
		reg.Register(dt)
	}
	reg.Permissions.LoadPermissionsFromDB(snapshot.Roles, snapshot.Permissions)

	dbName := siteCfg.DBName
	if err := schema.MigrateSiteFromRegistry(db, dbName, reg, dialect); err != nil {
		return fmt.Errorf("migration failed during rollback: %w", err)
	}
	// Run DDL before opening the transaction: MySQL may implicitly commit DDL.
	// The config snapshot and version are then restored atomically as DML.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rollback transaction: %w", err)
	}
	defer tx.Rollback()
	for _, dt := range snapshot.DocTypes {
		if err := store.SaveDocTypeTx(tx, dt, siteName); err != nil {
			return fmt.Errorf("saving doctype %s: %w", dt.Name, err)
		}
	}
	if len(snapshot.Roles) > 0 {
		if err := store.SaveRolesTx(tx, snapshot.Roles, siteName); err != nil {
			return fmt.Errorf("restoring roles: %w", err)
		}
	}
	if len(snapshot.Permissions) > 0 {
		if err := store.SavePermissionsTx(tx, snapshot.Permissions, siteName); err != nil {
			return fmt.Errorf("restoring permissions: %w", err)
		}
	}
	if err := configstore.BumpAuthorizationRevisionTx(tx, dialect, true, false); err != nil {
		return fmt.Errorf("advancing authorization revision during rollback: %w", err)
	}

	// Create a new Active version to record the rollback.
	if _, _, err := store.CreateConfigVersionTx(tx, siteName, "system", fmt.Sprintf("Rollback to version %d", toVersion), "Active", snapshot, targetVersionID); err != nil {
		return fmt.Errorf("recording rollback version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing rollback: %w", err)
	}

	fmt.Println("Rollback complete.")
	return nil
}
