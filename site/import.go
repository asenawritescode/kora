package site

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/asenawritescode/kora/query"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	sqlDialect "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/print"
	"github.com/asenawritescode/kora/schema"
	flow "github.com/asenawritescode/kora/workflow"
	workflowsql "github.com/asenawritescode/kora/workflow/sqlstore"
)

// ImportConfig parses YAML config from a directory, saves it to the database,
// builds the registry, and runs schema migration. This is a separate composable
// step called only when a YAML config directory is provided (CLI setup path).
// Console-created sites skip this — users configure doctypes via AI or admin UI.
func ImportConfig(db *sql.DB, registry *doctype.Registry, dbName, siteName, configPath string, dialect sqlDialect.Dialect) error {
	return ImportConfigForTenant(db, registry, dbName, siteName, siteName, configPath, dialect)
}

func ImportConfigForTenant(db *sql.DB, registry *doctype.Registry, dbName, siteName, tenantID, configPath string, dialect sqlDialect.Dialect) error {
	dirs, err := ResolvePackTree(configPath)
	if err != nil {
		return err
	}
	merged := &doctype.ConfigSnapshot{}
	names := map[string]bool{}
	for _, dir := range dirs {
		snapshot, err := parsePackSnapshot(dir)
		if err != nil {
			return err
		}
		if err := mergePackSnapshot(merged, snapshot, names); err != nil {
			return err
		}
	}
	if err := resolvePrintOperations(merged); err != nil {
		return err
	}
	return ImportConfigFromSnapshotForTenant(db, registry, dbName, siteName, tenantID, fmt.Sprintf("Config import from %s", configPath), merged, dialect, nil)
}

func parsePackSnapshot(configPath string) (*doctype.ConfigSnapshot, error) {
	// Step 1: Parse DocType config files.
	doctypes, err := doctype.ParseConfigTree(configPath)
	if err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Step 2: Parse roles and permissions.
	roles, permissions, err := doctype.ParseRolesDirectory(configPath)
	if err != nil {
		return nil, fmt.Errorf("parsing roles: %w", err)
	}

	// Load unified Engine v2 definitions from the pack's workflows directory.
	workflowDefinitions, err := ParseWorkflowDefinitionsDirectory(filepath.Join(configPath, "workflows"))
	if err != nil {
		return nil, fmt.Errorf("parsing Engine v2 workflow definitions: %w", err)
	}

	// Views live in the pack's dedicated views/ directory.
	views, err := doctype.ParseViewsDirectory(configPath + "/views")
	if err != nil {
		return nil, fmt.Errorf("parsing views: %w", err)
	}
	reportDefinitions, err := analytics.ParseReportsDirectory(configPath + "/reports")
	if err != nil {
		return nil, fmt.Errorf("parsing reports: %w", err)
	}
	commandDefinitions, err := kernel.ParseCommandDir(filepath.Join(configPath, "commands"))
	if err != nil {
		return nil, fmt.Errorf("parsing commands: %w", err)
	}
	commands, err := kernel.EncodeCommandResources(commandDefinitions)
	if err != nil {
		return nil, fmt.Errorf("encoding commands: %w", err)
	}
	queryDefs, err := query.LoadDir(filepath.Join(configPath, "queries"), registryOf(doctypes))
	if err != nil {
		return nil, fmt.Errorf("parsing queries: %w", err)
	}
	queries, err := query.Encode(queryDefs)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, d := range queryDefs {
		known[d.ModuleOwnerID+"/"+d.Name] = true
	}
	printFormats, err := loadPrintFormats(filepath.Join(configPath, "print_formats"), doctypes, known)
	if err != nil {
		return nil, err
	}
	reports := make([]json.RawMessage, 0, len(reportDefinitions))
	for _, report := range reportDefinitions {
		data, err := json.Marshal(report)
		if err != nil {
			return nil, fmt.Errorf("encoding report %s: %w", report.Name, err)
		}
		reports = append(reports, data)
	}

	snapshot := &doctype.ConfigSnapshot{
		DocTypes:            doctypes,
		Roles:               roles,
		Permissions:         permissions,
		WorkflowDefinitions: workflowDefinitions,
		Views:               views,
		Reports:             reports,
		Commands:            commands,
		PrintFormats:        printFormats,
		Queries:             queries,
	}
	manifest, ok, err := ParsePackFile(configPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		if len(queries) > 0 || hasOwnerTaggedCommand(commands) {
			return nil, fmt.Errorf("pack.manifest_required: %s has queries or module-owned commands but no %s", configPath, packFileName)
		}
		return snapshot, nil
	}
	if snapshot.Modules, err = GeneratePackModules(manifest, snapshot); err != nil {
		return nil, err
	}
	applyPackOwnership(snapshot, snapshot.Modules)
	return snapshot, nil
}

func hasOwnerTaggedCommand(commands []json.RawMessage) bool {
	for _, raw := range commands {
		var head struct {
			Owner string `json:"module_owner_id"`
		}
		if json.Unmarshal(raw, &head) == nil && head.Owner != "" {
			return true
		}
	}
	return false
}

// afterModuleRevisionRead is a test seam for the module CAS race window.
var afterModuleRevisionRead func()

func registryOf(dts []*doctype.DocType) *doctype.Registry {
	reg := doctype.NewRegistry()
	for _, dt := range dts {
		reg.Register(dt)
	}
	return reg
}

func ParseWorkflowDefinitionsDirectory(directory string) ([]contract.WorkflowDefinition, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	definitions := make([]contract.WorkflowDefinition, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		definition, err := flow.DecodeWorkflowDefinition(body)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

// ImportConfigFromSnapshot imports a full ConfigSnapshot into a site's database
// and runs schema migration. Unlike ImportConfig, it takes an in-memory snapshot
// instead of parsing YAML files from disk.
//
// This is the dynamic template path: templates store their ConfigSnapshot as
// JSON in the kora-content DB. When a user onboards with a template, the config
// is read from the DB and applied here — no filesystem dependency.
func ImportConfigFromSnapshot(db *sql.DB, registry *doctype.Registry, dbName, siteName, label string, snapshot *doctype.ConfigSnapshot, dialect sqlDialect.Dialect, scriptBodyByHash map[string]string) error {
	return ImportConfigFromSnapshotForTenant(db, registry, dbName, siteName, siteName, label, snapshot, dialect, scriptBodyByHash)
}

func PublishImportedWorkflowDefinitions(ctx context.Context, database *sql.DB, dialect sqlDialect.Dialect, tenantID string, definitions []contract.WorkflowDefinition, registry *doctype.Registry) error {
	tenant, err := contract.ParseTenantID(tenantID)
	if err != nil {
		return fmt.Errorf("invalid tenant ID: %w", err)
	}
	return publishImportedWorkflows(ctx, database, dialect, tenant, definitions, registry)
}

func publishImportedWorkflows(ctx context.Context, database *sql.DB, dialect sqlDialect.Dialect, tenant contract.TenantID, definitions []contract.WorkflowDefinition, registry *doctype.Registry) error {
	catalog := flow.NewCatalog()
	input := contract.SchemaRef{ID: "kora.notification.send", Version: 1}
	output := contract.SchemaRef{ID: "kora.notification.send.result", Version: 1}
	if err := catalog.Register(flow.ActionRegistration{Key: flow.ActionKey{Name: "notification.send", Version: 1}, Class: flow.ActionExternal, InputSchema: input, OutputSchema: output, RequiredGrant: policy.Grant{Resource: "workflow.notification", Operation: "submit"}, External: importedNotificationAction{}}); err != nil {
		return err
	}
	statusInput := contract.SchemaRef{ID: "kora.record.update_status", Version: 1}
	statusOutput := contract.SchemaRef{ID: "kora.record.update_status.result", Version: 1}
	if err := catalog.Register(flow.ActionRegistration{Key: flow.ActionKey{Name: "record.update_status", Version: 1}, Class: flow.ActionLocal, InputSchema: statusInput, OutputSchema: statusOutput, RequiredGrant: policy.Grant{Resource: "workflow.status", Operation: "submit"}, Local: importedRecordStatusAction{}}); err != nil {
		return err
	}
	if err := catalog.Freeze(); err != nil {
		return err
	}
	schemas := importedWorkflowSchemas{registry: registry}
	for _, definition := range definitions {
		validation := flow.ValidateDefinition(definition, catalog, schemas, nil)
		issues := append(validation.Issues, flow.ValidateRecordTriggerSchema(definition.Trigger, schemas)...)
		issues = append(issues, flow.ValidateRecordActionFields(definition, schemas)...)
		if definition.Trigger != nil {
			for j, step := range definition.Steps {
				if step.Condition != nil && step.Condition.Field != nil && !schemas.RecordFieldExists(definition.Trigger.Table, step.Condition.Field.Field) {
					issues = append(issues, contract.ValidationIssue{Path: fmt.Sprintf("steps[%d].condition.field", j), Code: "unknown_field", Detail: "condition field does not exist on the selected table"})
				}
			}
		}
		if len(issues) > 0 {
			return fmt.Errorf("definition %s issue %s: %s", definition.ID, issues[0].Path, issues[0].Detail)
		}
	}
	store, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := store.Ensure(ctx); err != nil {
		return err
	}
	actor, err := contract.ParsePrincipalID("system-config-import")
	if err != nil {
		return err
	}
	return store.PublishImportedBatch(ctx, tenant, actor, definitions, time.Now().UTC())
}

func configImportOperationID(siteName string, snapshot []byte) string {
	input := make([]byte, 0, len(siteName)+1+len(snapshot))
	input = append(input, siteName...)
	input = append(input, 0)
	input = append(input, snapshot...)
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:18])
}

type importedNotificationAction struct{}

func (importedNotificationAction) Execute(context.Context, flow.ActionContext, flow.ActionInput) (flow.ActionResult, error) {
	return flow.ActionResult{}, fmt.Errorf("import validation action cannot execute")
}

type importedRecordStatusAction struct{}

func (importedRecordStatusAction) ExecuteLocal(context.Context, flow.Claim, flow.ActionContext, flow.ActionKey, flow.ActionInput, contract.StepID) (flow.ActionResult, error) {
	return flow.ActionResult{}, fmt.Errorf("import validation action cannot execute")
}

type importedWorkflowSchemas struct{ registry *doctype.Registry }

func (s importedWorkflowSchemas) Registered(ref contract.SchemaRef) bool {
	return ref == (contract.SchemaRef{ID: "kora.notification.send", Version: 1}) || ref == (contract.SchemaRef{ID: "kora.notification.send.result", Version: 1}) || ref == (contract.SchemaRef{ID: "kora.record.update_status", Version: 1}) || ref == (contract.SchemaRef{ID: "kora.record.update_status.result", Version: 1})
}
func (s importedWorkflowSchemas) Validate(ref contract.SchemaRef, raw json.RawMessage) error {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return fmt.Errorf("workflow action data must be an object")
	}
	if ref == (contract.SchemaRef{ID: "kora.record.update_status", Version: 1}) {
		var config flow.StatusActionConfig
		if err := json.Unmarshal(raw, &config); err != nil {
			return fmt.Errorf("status action input is invalid")
		}
		if strings.TrimSpace(config.StateField) == "" || strings.TrimSpace(config.InitialState) == "" {
			return fmt.Errorf("status field and initial value are required")
		}
		if issues := flow.ValidateStatusTransition(config.StatusTransition); len(issues) > 0 {
			return fmt.Errorf("%s", issues[0].Detail)
		}
		return nil
	}
	if ref == (contract.SchemaRef{ID: "kora.record.update_status.result", Version: 1}) {
		return nil
	}
	if ref == (contract.SchemaRef{ID: "kora.notification.send", Version: 1}) {
		var channel, stateField, subject, message string
		var recipients []string
		if json.Unmarshal(value["channel"], &channel) != nil || (channel != "in_app" && channel != "email" && channel != "both") ||
			json.Unmarshal(value["recipient_fields"], &recipients) != nil || len(recipients) == 0 || len(recipients) > 50 ||
			json.Unmarshal(value["state_field"], &stateField) != nil || strings.TrimSpace(stateField) == "" ||
			json.Unmarshal(value["subject"], &subject) != nil || len(subject) > 200 ||
			json.Unmarshal(value["message"], &message) != nil || len(message) > 8192 {
			return fmt.Errorf("notification action input is invalid")
		}
		for key := range value {
			if key != "channel" && key != "recipient_fields" && key != "state_field" && key != "subject" && key != "message" {
				return fmt.Errorf("notification action input contains unsupported field %q", key)
			}
		}
		for _, field := range recipients {
			if strings.TrimSpace(field) == "" || strings.TrimSpace(field) != field {
				return fmt.Errorf("notification action recipient field is invalid")
			}
		}
		return nil
	}
	if ref == (contract.SchemaRef{ID: "kora.notification.send.result", Version: 1}) {
		var delivered int
		if len(value) != 1 || json.Unmarshal(value["delivered"], &delivered) != nil || delivered < 1 {
			return fmt.Errorf("notification action result is invalid")
		}
		return nil
	}
	return fmt.Errorf("notification action schema is not registered")
}
func (s importedWorkflowSchemas) RecordTableExists(table string) bool {
	return s.registry != nil && s.registry.Get(table) != nil
}
func (s importedWorkflowSchemas) RecordFieldExists(table, field string) bool {
	if s.registry == nil {
		return false
	}
	dt := s.registry.Get(table)
	return dt != nil && dt.GetField(field) != nil
}
func (s importedWorkflowSchemas) RecordFieldType(table, field string) (string, bool) {
	if s.registry == nil {
		return "", false
	}
	dt := s.registry.Get(table)
	if dt == nil {
		return "", false
	}
	f := dt.GetField(field)
	if f == nil {
		return "", false
	}
	if strings.EqualFold(f.Fieldtype, "Data") && strings.EqualFold(strings.TrimSpace(f.Options), "Email") {
		return "Email", true
	}
	return f.Fieldtype, true
}
func (s importedWorkflowSchemas) RecordFieldOptions(table, field string) (string, bool) {
	if s.registry == nil {
		return "", false
	}
	dt := s.registry.Get(table)
	if dt == nil {
		return "", false
	}
	definition := dt.GetField(field)
	if definition == nil {
		return "", false
	}
	return definition.Options, true
}

func ImportConfigFromSnapshotForTenant(db *sql.DB, registry *doctype.Registry, dbName, siteName, tenantID, label string, snapshot *doctype.ConfigSnapshot, dialect sqlDialect.Dialect, scriptBodyByHash map[string]string) error {
	return ImportConfigFromSnapshotContext(context.Background(), db, registry, dbName, siteName, tenantID, label, snapshot, dialect, scriptBodyByHash)
}

// ImportConfigFromSnapshotContext activates a complete config snapshot through
// the same journaled, revision-fenced transaction used by CLI setup/import.
func ImportConfigFromSnapshotContext(ctx context.Context, database *sql.DB, registry *doctype.Registry, dbName, siteName, tenantID, label string, snapshot *doctype.ConfigSnapshot, dialect sqlDialect.Dialect, scriptBodyByHash map[string]string) (returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if database == nil || registry == nil || dialect == nil || dbName == "" || siteName == "" || label == "" {
		return fmt.Errorf("config activation requires database, registry, dialect, database name, site, and label")
	}
	if snapshot == nil {
		return fmt.Errorf("snapshot is required")
	}
	tenant, err := contract.ParseTenantID(tenantID)
	if err != nil {
		return fmt.Errorf("invalid workflow tenant ID: %w", err)
	}
	if _, err := kernel.DecodeCommandResources(snapshot.Commands); err != nil {
		return fmt.Errorf("invalid command definitions: %w", err)
	}
	for _, raw := range snapshot.PrintFormats {
		if _, err := print.Decode(raw); err != nil {
			return fmt.Errorf("invalid print format: %w", err)
		}
	}
	if _, err := query.Decode(snapshot.Queries, registryOf(snapshot.DocTypes)); err != nil {
		return fmt.Errorf("invalid query definitions: %w", err)
	}
	workflowDefinitions := append([]contract.WorkflowDefinition(nil), snapshot.WorkflowDefinitions...)
	store := configstore.NewStore(database, dialect)
	liveDocTypes, err := store.LoadAll(siteName)
	if err != nil {
		return fmt.Errorf("loading existing doctypes for reference validation: %w", err)
	}
	liveRegistry := doctype.NewRegistry()
	if registry != nil {
		liveRegistry.LoadFull(registry.All(), nil, nil)
		if registry.Views != nil {
			liveRegistry.Views.LoadFromDB(registry.Views.All())
		}
	}
	liveRegistry.LoadFull(liveDocTypes, nil, nil)
	if findings := doctype.ValidateSnapshotReferences(snapshot, liveRegistry); len(findings) > 0 {
		return fmt.Errorf("configuration references are invalid: %s at %s targets %q", findings[0].Code, findings[0].Path, findings[0].Target)
	}
	snapshotCopy := *snapshot
	snapshotCopy.WorkflowDefinitions = workflowDefinitions
	snapshotBytes, err := json.Marshal(&snapshotCopy)
	if err != nil {
		return fmt.Errorf("encoding config activation snapshot: %w", err)
	}
	expectedRevision, err := configstore.CurrentConfigRevision(ctx, database, dialect)
	if err != nil {
		return fmt.Errorf("reading config revision for activation preflight: %w", err)
	}
	// MySQL DDL commits implicitly, so module tables exist before BeginTx.
	moduleStore := configstore.NewModuleStore(database, dialect)
	var moduleRevision uint64
	var moduleDigest string
	commitModules := false
	if len(snapshotCopy.Modules) > 0 {
		if err := moduleStore.EnsureSchema(); err != nil {
			return fmt.Errorf("ensuring module snapshot schema: %w", err)
		}
		current, err := moduleStore.Load(tenant)
		if err != nil {
			return fmt.Errorf("loading module snapshot: %w", err)
		}
		moduleRevision = current.Revision
		modulesJSON, err := json.Marshal(snapshotCopy.Modules)
		if err != nil {
			return fmt.Errorf("encoding module descriptors: %w", err)
		}
		sum := sha256.Sum256(modulesJSON)
		moduleDigest = hex.EncodeToString(sum[:])
		installed := map[contract.ModuleID]string{}
		for _, in := range current.Installations {
			installed[in.ModuleID] = in.ArtifactDigest
		}
		for _, m := range snapshotCopy.Modules {
			if installed[m.ID] != moduleDigest {
				commitModules = true
			}
		}
		if afterModuleRevisionRead != nil {
			afterModuleRevisionRead()
		}
	}
	operationID := configImportOperationID(siteName, snapshotBytes)
	operation, err := configstore.EnsureActivationOperation(database, dialect, siteName, operationID, "import", &snapshotCopy)
	if err != nil {
		return fmt.Errorf("recording config activation operation: %w", err)
	}
	if operation.Phase == configstore.ActivationActivated {
		return fmt.Errorf("config activation operation %s has an invalid activated phase", operationID)
	}
	if operation.Phase != configstore.ActivationCompleted && operation.Phase != configstore.ActivationValidated {
		if err := configstore.UpdateActivationOperation(database, dialect, siteName, operationID, "import", configstore.ActivationApplying, operation.DDLPlan, "resume-from-live-schema", ""); err != nil {
			return fmt.Errorf("starting config activation operation: %w", err)
		}
		operation.Phase = configstore.ActivationApplying
	}
	journalPhase := operation.Phase
	defer func() {
		if returnErr == nil || journalPhase == configstore.ActivationCompleted {
			return
		}
		if err := configstore.UpdateActivationOperation(database, dialect, siteName, operationID, "import", journalPhase, operation.DDLPlan, operation.RecoveryPlan, returnErr.Error()); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("recording config activation failure: %w", err))
		}
	}()

	versionID := strings.TrimSpace(operation.RecoveryPlan)
	if operation.Phase != configstore.ActivationValidated && operation.Phase != configstore.ActivationCompleted {
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin config activation transaction: %w", err)
		}
		defer tx.Rollback()
		if err := store.ActivateSnapshotAtConfigRevision(tx, &snapshotCopy, siteName, dialect, expectedRevision); err != nil {
			return fmt.Errorf("activating config snapshot at expected revision %d: %w", expectedRevision, err)
		}
		if commitModules {
			if err := store.CommitModulesTx(ctx, tx, dialect, tenant, snapshotCopy.Modules, moduleDigest, moduleRevision, nil, nil); err != nil {
				return fmt.Errorf("committing pack modules: %w", err)
			}
		}
		var version int
		versionID, version, err = store.CreateConfigVersionTx(tx, siteName, "system", label, "Active", &snapshotCopy, "")
		if err != nil {
			return fmt.Errorf("creating config version: %w", err)
		}
		if err := configstore.UpdateActivationOperationPhaseTx(tx, dialect, siteName, operationID, "import", configstore.ActivationApplying, configstore.ActivationValidated, versionID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing config activation: %w", err)
		}
		journalPhase = configstore.ActivationValidated
		operation.RecoveryPlan = versionID
		fmt.Printf("  ✓ Config version %d (%s) created\n", version, versionID)
	} else if versionID == "" {
		return fmt.Errorf("config activation operation %s is missing its committed version reference", operationID)
	}

	registry.LoadFull(snapshotCopy.DocTypes, snapshotCopy.Roles, snapshotCopy.Permissions)
	registry.Views.LoadFromDB(snapshotCopy.Views)
	if err := schema.MigrateSiteFromRegistry(database, dbName, registry, dialect); err != nil {
		return fmt.Errorf("migrating schema: %w", err)
	}
	if len(workflowDefinitions) > 0 {
		if err := publishImportedWorkflows(ctx, database, dialect, tenant, workflowDefinitions, registry); err != nil {
			return fmt.Errorf("publishing Engine v2 workflows: %w", err)
		}
	}
	if err := configstore.UpdateActivationOperation(database, dialect, siteName, operationID, "import", configstore.ActivationCompleted, operation.DDLPlan, versionID, ""); err != nil {
		return fmt.Errorf("finalizing config activation operation: %w", err)
	}
	journalPhase = configstore.ActivationCompleted

	// Print changelog summary.
	changelogStr, err := readChangelog(ctx, database, dialect, versionID)
	if err != nil {
		fmt.Printf("Warning: config was imported, but its changelog summary could not be read: %v\n", err)
		return nil
	}
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

// readChangelog returns a version's changelog as text. NULL is scanned rather
// than COALESCEd to ” because JSON/JSONB columns reject an empty-string literal.
func readChangelog(ctx context.Context, database *sql.DB, dialect sqlDialect.Dialect, versionID string) (string, error) {
	var changelog sql.NullString
	err := database.QueryRowContext(ctx, sqlDialect.Rebind(dialect, "SELECT changelog FROM _kora_config_version WHERE id = ?"), versionID).Scan(&changelog)
	return changelog.String, err
}

// PackFile preserves the legacy site-package name for the versioned contract.
type PackFile = contract.PackFile

// ParseConfigPack validates and parses a template pack without making database
// or process-local registry changes. Callers can persist the returned snapshot
// through the durable configuration activation path.
func ParseConfigPack(packName string, files []PackFile) (*doctype.ConfigSnapshot, map[string]string, error) {
	snapshot, scripts, err := parseConfigPack(packName, files)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		if f.Path != packFileName {
			continue
		}
		manifest, _, err := parsePackManifest([]byte(f.Content), packName)
		if err != nil {
			return nil, nil, err
		}
		if snapshot.Modules, err = GeneratePackModules(manifest, snapshot); err != nil {
			return nil, nil, err
		}
	}
	return snapshot, scripts, nil
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
	return ImportConfigFromPackForTenant(db, registry, dbName, siteName, siteName, packName, files, dialect)
}

func ImportConfigFromPackForTenant(db *sql.DB, registry *doctype.Registry, dbName, siteName, tenantID, packName string, files []PackFile, dialect sqlDialect.Dialect) error {
	snapshot, scriptBodyByHash, err := parseConfigPack(packName, files)
	if err != nil {
		return err
	}
	return ImportConfigFromSnapshotForTenant(db, registry, dbName, siteName, tenantID,
		fmt.Sprintf("Template pack %q import", packName), snapshot, dialect, scriptBodyByHash)
}

func parseConfigPack(packName string, files []PackFile) (*doctype.ConfigSnapshot, map[string]string, error) {
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("pack has no files")
	}

	const maxFileSize = 256 * 1024       // 256 KB per file
	const maxTotalSize = 2 * 1024 * 1024 // 2 MB total pack

	var doctypes []*doctype.DocType
	var roles []*doctype.Role
	var permissions []*doctype.Permission
	var workflowDefinitions []contract.WorkflowDefinition
	var views []*doctype.View
	var reports []json.RawMessage
	var commandDefs []*kernel.CommandResource
	var commandPaths []string
	var queryFiles, printFiles []PackFile
	var scripts []*doctype.ScriptSnapshot
	scriptBodyByHash := make(map[string]string)
	var totalSize int

	for _, f := range files {
		f.Path = strings.TrimSpace(f.Path)
		f.ContentType = strings.TrimSpace(f.ContentType)
		if f.Path == packFileName {
			continue // manifest, consumed by ParseConfigPack
		}

		// ── Path security ──────────────────────────────────────────
		if f.Path == "" {
			return nil, nil, fmt.Errorf("pack file has empty path")
		}
		if filepath.IsAbs(f.Path) || strings.Contains(f.Path, "..") {
			return nil, nil, fmt.Errorf("pack file %q: path traversal or absolute path rejected", f.Path)
		}

		// ── Extension validation ───────────────────────────────────
		ext := strings.ToLower(filepath.Ext(f.Path))
		if f.ContentType == "script" {
			if ext != ".js" {
				return nil, nil, fmt.Errorf("pack file %q: script files must be .js, got %q", f.Path, ext)
			}
		} else if f.ContentType == "workflow_definition" {
			if ext != ".json" {
				return nil, nil, fmt.Errorf("pack file %q: workflow_definition files must be .json, got %q", f.Path, ext)
			}
		} else if ext != ".yaml" && ext != ".yml" {
			return nil, nil, fmt.Errorf("pack file %q: only .yaml/.yml allowed for %q content, got %q", f.Path, f.ContentType, ext)
		}
		if ext == ".yml" && f.ContentType == "doctype" && !strings.HasPrefix(f.Path, "doctypes/") {
			return nil, nil, fmt.Errorf("pack file %q: doctype files must be under doctypes/", f.Path)
		}

		// ── Content_type ↔ path agreement ──────────────────────────
		switch f.ContentType {
		case "doctype":
			if !strings.HasPrefix(f.Path, "doctypes/") {
				return nil, nil, fmt.Errorf("pack file %q: content_type doctype requires path under doctypes/", f.Path)
			}
		case "roles":
			if f.Path != "roles.yaml" && !(strings.HasPrefix(f.Path, "components/") && filepath.Base(f.Path) == "roles.yaml") {
				return nil, nil, fmt.Errorf("pack file %q: content_type roles requires roles.yaml at package root or under components/", f.Path)
			}
		case "permissions":
			if f.Path != "permissions.yaml" && !(strings.HasPrefix(f.Path, "components/") && filepath.Base(f.Path) == "permissions.yaml") {
				return nil, nil, fmt.Errorf("pack file %q: content_type permissions requires permissions.yaml at package root or under components/", f.Path)
			}
		case "workflow_definition":
			if !strings.HasPrefix(f.Path, "workflows/") {
				return nil, nil, fmt.Errorf("pack file %q: content_type workflow_definition requires path under workflows/", f.Path)
			}
		case "view":
			if !strings.HasPrefix(f.Path, "views/") {
				return nil, nil, fmt.Errorf("pack file %q: content_type view requires path under views/", f.Path)
			}
		case "report":
			if !strings.HasPrefix(f.Path, "reports/") {
				return nil, nil, fmt.Errorf("pack file %q: content_type report requires path under reports/", f.Path)
			}
		case "query", "command", "print_format":
			if dir := map[string]string{"query": "queries/", "command": "commands/", "print_format": "print_formats/"}[f.ContentType]; !strings.HasPrefix(f.Path, dir) {
				return nil, nil, fmt.Errorf("pack file %q: content_type %s requires path under %s", f.Path, f.ContentType, dir)
			}
		case "script":
			if !strings.HasPrefix(f.Path, "scripts/") {
				return nil, nil, fmt.Errorf("pack file %q: content_type script requires path under scripts/", f.Path)
			}
			ext := strings.ToLower(filepath.Ext(f.Path))
			if ext != ".js" {
				return nil, nil, fmt.Errorf("pack file %q: script files must be .js, got %q", f.Path, ext)
			}
		default:
			return nil, nil, fmt.Errorf("pack file %q: unknown content_type %q (allowed: doctype, roles, permissions, workflow_definition, view, report, query, command, print_format, script)", f.Path, f.ContentType)
		}

		// ── Size limits ────────────────────────────────────────────
		fileSize := len(f.Content)
		if fileSize > maxFileSize {
			return nil, nil, fmt.Errorf("pack file %q: size %d exceeds max %d bytes", f.Path, fileSize, maxFileSize)
		}
		totalSize += fileSize
		if totalSize > maxTotalSize {
			return nil, nil, fmt.Errorf("pack total size %d exceeds max %d bytes", totalSize, maxTotalSize)
		}

		// ── Parse ──────────────────────────────────────────────────
		data := []byte(f.Content)
		switch f.ContentType {
		case "doctype":
			dt, err := doctype.ParseYAML(data, f.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			doctypes = append(doctypes, dt)

		case "roles":
			r, err := doctype.ParseRolesYAML(data)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			roles = append(roles, r...)

		case "permissions":
			p, err := doctype.ParsePermissionsYAML(data)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			permissions = append(permissions, p...)

		case "workflow_definition":
			definition, err := flow.DecodeWorkflowDefinition(data)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: parse Engine v2 workflow definition: %w", f.Path, err)
			}
			workflowDefinitions = append(workflowDefinitions, definition)

		case "view":
			v, err := doctype.ParseViewYAML(data)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			views = append(views, v)

		case "report":
			report, err := analytics.ParseReportYAML(data, f.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: encoding report: %w", f.Path, err)
			}
			reports = append(reports, encoded)

		case "command":
			def, err := kernel.ParseCommandResource(data)
			if err != nil {
				return nil, nil, fmt.Errorf("pack file %q: %w", f.Path, err)
			}
			commandDefs = append(commandDefs, def)
			commandPaths = append(commandPaths, f.Path)

		case "query":
			queryFiles = append(queryFiles, f)

		case "print_format":
			printFiles = append(printFiles, f)

		case "script":
			h := sha256.Sum256(data)
			doctypeName, eventName := packScriptBinding(f.Path)
			scriptName := strings.TrimSuffix(filepath.Base(f.Path), ".js")
			if doctypeName != "" {
				scriptName = strings.ReplaceAll(doctypeName, " ", "_") + "_" + eventName
			}
			sc := &doctype.ScriptSnapshot{
				Name:       scriptName,
				ScriptType: "doc_event",
				DocType:    doctypeName,
				Event:      eventName,
				IsActive:   true,
				Priority:   10,
				TimeoutMs:  5000,
				ScriptHash: hex.EncodeToString(h[:]),
			}
			scripts = append(scripts, sc)
			scriptBodyByHash[sc.ScriptHash] = f.Content
		}
	}

	commands, queries, printFormats, err := parsePackTail(doctypes, commandDefs, commandPaths, queryFiles, printFiles)
	if err != nil {
		return nil, nil, err
	}
	return &doctype.ConfigSnapshot{
		DocTypes:            doctypes,
		Roles:               roles,
		Permissions:         permissions,
		WorkflowDefinitions: workflowDefinitions,
		Views:               views,
		Reports:             reports,
		Commands:            commands,
		Queries:             queries,
		PrintFormats:        printFormats,
		Scripts:             scripts,
	}, scriptBodyByHash, nil
}

// packScriptBinding supports an explicit scripts/<DocType>/<event>.js path.
// Flat scripts/<name>.js packages keep their historic unbound metadata.
func packScriptBinding(path string) (doctypeName, eventName string) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) != 3 || parts[0] != "scripts" || parts[1] == "" {
		return "", ""
	}
	eventName = strings.TrimSuffix(parts[2], ".js")
	switch eventName {
	case "before_insert", "after_insert", "before_save", "after_save", "before_delete", "after_delete", "before_submit", "after_submit", "before_cancel", "after_cancel", "validate", "computed", "payment":
		return parts[1], eventName
	default:
		return "", ""
	}
}

// parsePackTail encodes package commands, queries and print formats the way
// parsePackSnapshot does for a config tree, sorted by path like import sorts
// by file name. Queries and print formats are checked against the DocTypes.
func parsePackTail(doctypes []*doctype.DocType, cmds []*kernel.CommandResource, cmdPaths []string, queryFiles, printFiles []PackFile) (commands, queries, printFormats []json.RawMessage, err error) {
	idx := make([]int, len(cmds))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return cmdPaths[idx[a]] < cmdPaths[idx[b]] })
	sorted := make([]*kernel.CommandResource, 0, len(cmds))
	for _, i := range idx {
		sorted = append(sorted, cmds[i])
	}
	if commands, err = kernel.EncodeCommandResources(sorted); err != nil {
		return nil, nil, nil, fmt.Errorf("encoding commands: %w", err)
	}
	sort.Slice(queryFiles, func(a, b int) bool { return queryFiles[a].Path < queryFiles[b].Path })
	reg := registryOf(doctypes)
	var defs []*query.Definition
	known := map[string]bool{}
	for _, f := range queryFiles {
		d, err := query.Parse([]byte(f.Content), reg)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("pack file %q: %w", f.Path, err)
		}
		defs = append(defs, d)
		known[d.ModuleOwnerID+"/"+d.Name] = true
	}
	if queries, err = query.Encode(defs); err != nil {
		return nil, nil, nil, err
	}
	sort.Slice(printFiles, func(a, b int) bool { return printFiles[a].Path < printFiles[b].Path })
	if printFormats, err = encodePrintFormats(printFiles, doctypes, known); err != nil {
		return nil, nil, nil, err
	}
	return commands, queries, printFormats, nil
}
