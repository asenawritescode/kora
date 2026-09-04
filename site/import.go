package site

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/asenawritescode/kora/analytics"
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

	// Views live in the pack's dedicated views/ directory.
	views, err := doctype.ParseViewsDirectory(configPath + "/views")
	if err != nil {
		return fmt.Errorf("parsing views: %w", err)
	}
	reportDefinitions, err := analytics.ParseReportsDirectory(configPath + "/reports")
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

	return ImportConfigFromSnapshot(db, registry, dbName, siteName, fmt.Sprintf("Config import from %s", configPath), &doctype.ConfigSnapshot{
		DocTypes:    doctypes,
		Roles:       roles,
		Permissions: permissions,
		Workflows:   workflows,
		Views:       views,
		Reports:     reports,
	}, dialect, nil)
}

// ImportConfigFromSnapshot imports a full ConfigSnapshot into a site's database
// and runs schema migration. Unlike ImportConfig, it takes an in-memory snapshot
// instead of parsing YAML files from disk.
//
// This is the dynamic template path: templates store their ConfigSnapshot as
// JSON in the kora-content DB. When a user onboards with a template, the config
// is read from the DB and applied here — no filesystem dependency.
func ImportConfigFromSnapshot(db *sql.DB, registry *doctype.Registry, dbName, siteName, label string, snapshot *doctype.ConfigSnapshot, dialect sqlDialect.Dialect, scriptBodyByHash map[string]string) error {
	if snapshot == nil {
		return fmt.Errorf("snapshot is required")
	}

	// Step 1: Save to database.
	store := configstore.NewStore(db, dialect)
	for _, dt := range snapshot.DocTypes {
		if err := store.SaveDocType(dt, siteName); err != nil {
			return fmt.Errorf("saving doctype %s: %w", dt.Name, err)
		}
	}
	if err := store.SaveRoles(snapshot.Roles, siteName); err != nil {
		return fmt.Errorf("saving roles: %w", err)
	}
	if err := store.SavePermissions(snapshot.Permissions, siteName); err != nil {
		return fmt.Errorf("saving permissions: %w", err)
	}
	if err := store.SaveWorkflows(snapshot.Workflows, siteName); err != nil {
		return fmt.Errorf("saving workflows: %w", err)
	}
	if err := store.SaveViews(snapshot.Views, siteName); err != nil {
		return fmt.Errorf("saving views: %w", err)
	}
	if err := store.SaveAnalyticsMetrics(snapshot.AnalyticsMetrics, siteName); err != nil {
		return fmt.Errorf("saving analytics metrics: %w", err)
	}
	if err := store.SaveScripts(snapshot.Scripts, scriptBodyByHash, siteName); err != nil {
		return fmt.Errorf("saving scripts: %w", err)
	}

	// Step 2: Build registry with full config.
	registry.LoadFull(snapshot.DocTypes, snapshot.Roles, snapshot.Permissions)
	for _, wf := range snapshot.Workflows {
		registry.Workflows.Register(wf)
	}

	// Step 3: Create config version BEFORE migration.
	versionID, _, err := store.CreateConfigVersion(siteName, "system", label, "Active", snapshot)
	if err != nil {
		return fmt.Errorf("creating config version: %w", err)
	}

	// Step 4: Run schema migration.
	if err := schema.MigrateSiteFromRegistry(db, dbName, registry, dialect); err != nil {
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

// PackFile represents a single file in a CMS-backed template pack.
type PackFile struct {
	Path        string // relative path (e.g., "doctypes/patient.yaml", "views/dashboard.yaml", "roles.yaml")
	Content     string // file content (YAML for configs, JavaScript for scripts)
	ContentType string // "doctype", "roles", "permissions", "workflow", "view", "script"
}

// ImportConfigFromPack imports a template pack from in-memory YAML files.
// This is the CMS-backed template path: pack files are stored as child rows
// in the kora-cms Template Pack doctype. No filesystem dependency.
//
// Security gates enforced here:
//   - Rejects path traversal ("..", absolute paths)
//   - Only .yaml/.yml extensions allowed
//   - extension must agree with content_type (e.g., doctypes/*.yaml → "doctype")
//   - individual file size cap (256 KB)
//   - total pack size cap (2 MB)
//   - blocks content_types that don't belong in packs (scripts, webhooks)
func ImportConfigFromPack(db *sql.DB, registry *doctype.Registry, dbName, siteName, packName string, files []PackFile, dialect sqlDialect.Dialect) error {
	if len(files) == 0 {
		return fmt.Errorf("pack has no files")
	}

	const maxFileSize = 256 * 1024       // 256 KB per file
	const maxTotalSize = 2 * 1024 * 1024 // 2 MB total pack

	var doctypes []*doctype.DocType
	var roles []*doctype.Role
	var permissions []*doctype.Permission
	var workflows []*doctype.Workflow
	var views []*doctype.View
	var scripts []*doctype.ScriptSnapshot
	scriptBodyByHash := make(map[string]string)
	var totalSize int

	for _, f := range files {
		f.Path = strings.TrimSpace(f.Path)
		f.ContentType = strings.TrimSpace(f.ContentType)

		// ── Path security ──────────────────────────────────────────
		if f.Path == "" {
			return fmt.Errorf("pack file has empty path")
		}
		if filepath.IsAbs(f.Path) || strings.Contains(f.Path, "..") {
			return fmt.Errorf("pack file %q: path traversal or absolute path rejected", f.Path)
		}

		// ── Extension validation ───────────────────────────────────
		ext := strings.ToLower(filepath.Ext(f.Path))
		if ext != ".yaml" && ext != ".yml" {
			return fmt.Errorf("pack file %q: only .yaml/.yml allowed, got %q", f.Path, ext)
		}
		if ext == ".yml" && f.ContentType == "doctype" && !strings.HasPrefix(f.Path, "doctypes/") {
			return fmt.Errorf("pack file %q: doctype files must be under doctypes/", f.Path)
		}

		// ── Content_type ↔ path agreement ──────────────────────────
		switch f.ContentType {
		case "doctype":
			if !strings.HasPrefix(f.Path, "doctypes/") {
				return fmt.Errorf("pack file %q: content_type doctype requires path under doctypes/", f.Path)
			}
		case "roles":
			if f.Path != "roles.yaml" {
				return fmt.Errorf("pack file %q: content_type roles requires path roles.yaml", f.Path)
			}
		case "permissions":
			if f.Path != "permissions.yaml" {
				return fmt.Errorf("pack file %q: content_type permissions requires path permissions.yaml", f.Path)
			}
		case "workflow":
			if !strings.Contains(f.Path, "workflow") {
				return fmt.Errorf("pack file %q: content_type workflow requires a workflow path", f.Path)
			}
		case "view":
			if !strings.HasPrefix(f.Path, "views/") {
				return fmt.Errorf("pack file %q: content_type view requires path under views/", f.Path)
			}
		case "script":
			if !strings.HasPrefix(f.Path, "scripts/") {
				return fmt.Errorf("pack file %q: content_type script requires path under scripts/", f.Path)
			}
			ext := strings.ToLower(filepath.Ext(f.Path))
			if ext != ".js" {
				return fmt.Errorf("pack file %q: script files must be .js, got %q", f.Path, ext)
			}
		default:
			return fmt.Errorf("pack file %q: unknown content_type %q (allowed: doctype, roles, permissions, workflow, view, script)", f.Path, f.ContentType)
		}

		// ── Size limits ────────────────────────────────────────────
		fileSize := len(f.Content)
		if fileSize > maxFileSize {
			return fmt.Errorf("pack file %q: size %d exceeds max %d bytes", f.Path, fileSize, maxFileSize)
		}
		totalSize += fileSize
		if totalSize > maxTotalSize {
			return fmt.Errorf("pack total size %d exceeds max %d bytes", totalSize, maxTotalSize)
		}

		// ── Parse ──────────────────────────────────────────────────
		data := []byte(f.Content)
		switch f.ContentType {
		case "doctype":
			dt, err := doctype.ParseYAML(data, f.Path)
			if err != nil {
				return fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			doctypes = append(doctypes, dt)

		case "roles":
			r, err := doctype.ParseRolesYAML(data)
			if err != nil {
				return fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			roles = append(roles, r...)

		case "permissions":
			p, err := doctype.ParsePermissionsYAML(data)
			if err != nil {
				return fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			permissions = append(permissions, p...)

		case "workflow":
			wf, err := doctype.ParseWorkflowYAML(data)
			if err != nil {
				return fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			workflows = append(workflows, wf)

		case "view":
			v, err := doctype.ParseViewYAML(data)
			if err != nil {
				return fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			views = append(views, v)

		case "script":
			h := sha256.Sum256(data)
			sc := &doctype.ScriptSnapshot{
				Name:       strings.TrimSuffix(filepath.Base(f.Path), ".js"),
				ScriptType: "doc_event",
				IsActive:   true,
				Priority:   10,
				TimeoutMs:  5000,
				ScriptHash: hex.EncodeToString(h[:]),
			}
			scripts = append(scripts, sc)
			scriptBodyByHash[sc.ScriptHash] = f.Content
		}
	}

	return ImportConfigFromSnapshot(db, registry, dbName, siteName,
		fmt.Sprintf("Template pack %q import", packName),
		&doctype.ConfigSnapshot{
			DocTypes:    doctypes,
			Roles:       roles,
			Permissions: permissions,
			Workflows:   workflows,
			Views:       views,
			Scripts:     scripts,
		}, dialect, scriptBodyByHash)
}
