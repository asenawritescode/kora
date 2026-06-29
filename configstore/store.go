// Package configstore manages reading and writing DocType configuration
// to/from the database (_kora_doctype and _kora_field tables).
package configstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// Store persists DocType configurations to the database.
type Store struct {
	DB      *sql.DB
	Dialect db.QueryDialect
}

// NewStore creates a new config store.
func NewStore(database *sql.DB, dialect db.QueryDialect) *Store {
	return &Store{DB: database, Dialect: dialect}
}

// SaveDocType inserts or updates a DocType and its fields in the database.
// Uses a transaction for atomicity. Existing fields are diffed against new fields:
// only removed fields are deleted, only new fields are inserted, changed fields are updated.
// SaveDocTypeTx is like SaveDocType but uses an existing transaction for atomic multi-doctype saves.
func (s *Store) SaveDocTypeTx(tx *sql.Tx, dt *doctype.DocType, site string) error {
	return s.saveDocTypeExec(tx, dt, site)
}

func (s *Store) SaveDocType(dt *doctype.DocType, site string) error {
	dbTx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer dbTx.Rollback()
	if err := s.saveDocTypeExec(dbTx, dt, site); err != nil {
		return err
	}
	return dbTx.Commit()
}

func (s *Store) saveDocTypeExec(ex db.Queryer, dt *doctype.DocType, site string) error {
	configJSON, err := json.Marshal(dt)
	if err != nil {
		return fmt.Errorf("marshaling doctype: %w", err)
	}

	// Upsert the DocType record.
	doctypeSQL := fmt.Sprintf(
		"INSERT INTO _kora_doctype (name, module, is_submittable, is_child_table, is_single, track_changes, title_field, search_fields, sort_field, sort_order, description, config_json, version, site) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?) %s",
		s.Dialect.UpsertClause([]string{"name"}, []string{"module", "is_submittable", "is_child_table", "is_single", "track_changes", "title_field", "search_fields", "sort_field", "sort_order", "description", "config_json", "site"}),
	)
	_, err = ex.Exec(doctypeSQL,
		dt.Name, dt.Module, boolToInt(dt.IsSubmittable), boolToInt(dt.IsChildTable),
		boolToInt(dt.IsSingle), boolToInt(dt.TrackChanges),
		dt.TitleField, dt.SearchFields, dt.SortField, dt.SortOrder,
		dt.Description, string(configJSON), site,
	)
	if err != nil {
		return fmt.Errorf("saving doctype %s: %w", dt.Name, err)
	}

	if len(dt.Fields) == 0 {
		// No fields — delete any stale rows and we're done.
		_, err = ex.Exec("DELETE FROM _kora_field WHERE parent = ? AND site = ?", dt.Name, site)
		if err != nil {
			return fmt.Errorf("deleting fields for %s: %w", dt.Name, err)
		}
		return nil
	}

	// Delete all existing fields for this doctype in one shot,
	// then re-insert all current fields with a batched multi-row INSERT.
	// This replaces the old per-field diff/update/insert loop.
	_, err = ex.Exec("DELETE FROM _kora_field WHERE parent = ? AND site = ?", dt.Name, site)
	if err != nil {
		return fmt.Errorf("deleting fields for %s: %w", dt.Name, err)
	}

	// Batch-insert fields in groups to stay under SQLite's max variable limit (999).
	// 23 columns per field → 40 fields per batch = 920 params.
	const batchSize = 40
	for start := 0; start < len(dt.Fields); start += batchSize {
		end := min(start+batchSize, len(dt.Fields))
		batch := dt.Fields[start:end]

		placeholders := make([]string, 0, len(batch))
		args := make([]any, 0, len(batch)*23)
		for j, field := range batch {
			constraintsJSON, _ := json.Marshal(field.Constraints)
			placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args,
				fmt.Sprintf("%s.%s", dt.Name, field.Fieldname),
				dt.Name, field.Fieldname, field.Fieldtype, field.Label, field.Options,
				boolToInt(field.Reqd), boolToInt(field.Unique), field.Default,
				boolToInt(field.Hidden), boolToInt(field.ReadOnly), boolToInt(field.Bold),
				boolToInt(field.InListView), boolToInt(field.InStandardFilter),
				boolToInt(field.SearchIndex), field.Description,
				field.DependsOn, field.MandatoryDependsOn, string(constraintsJSON),
				field.RenamedFrom, field.LinkedField, field.Computed, start+j,
			)
		}

		insertSQL := fmt.Sprintf(
			`INSERT INTO _kora_field (name, parent, fieldname, fieldtype, label, options,
				reqd, unique_constraint, default_value, hidden, read_only, bold,
				in_list_view, in_standard_filter, search_index, description,
				depends_on, mandatory_depends_on, constraints_json, renamed_from, linked_field, computed, idx)
			VALUES %s`,
			strings.Join(placeholders, ", "),
		)
		if _, err := ex.Exec(insertSQL, args...); err != nil {
			return fmt.Errorf("inserting fields for %s: %w", dt.Name, err)
		}
	}

	slog.Debug("saved doctype config", "name", dt.Name, "fields", len(dt.Fields))
	return nil
}

// LoadAll loads all DocTypes from the database into the registry.
// Uses two batched queries instead of N+1 (one for doctypes, one for all fields).
func (s *Store) LoadAll(site string) ([]*doctype.DocType, error) {
	rows, err := s.DB.Query(`
		SELECT name, module, is_submittable, is_child_table, is_single,
			track_changes, title_field, search_fields, sort_field, sort_order, description
		FROM _kora_doctype
		WHERE site = ? OR site = ''
		ORDER BY name
	`, site)
	if err != nil {
		return nil, fmt.Errorf("querying doctypes: %w", err)
	}
	defer rows.Close()

	var doctypes []*doctype.DocType
	for rows.Next() {
		dt := &doctype.DocType{}
		var isSubmittable, isChildTable, isSingle, trackChanges int
		err := rows.Scan(
			&dt.Name, &dt.Module, &isSubmittable, &isChildTable, &isSingle,
			&trackChanges, &dt.TitleField, &dt.SearchFields, &dt.SortField,
			&dt.SortOrder, &dt.Description,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning doctype: %w", err)
		}
		dt.IsSubmittable = isSubmittable == 1
		dt.IsChildTable = isChildTable == 1
		dt.IsSingle = isSingle == 1
		dt.TrackChanges = trackChanges == 1
		doctypes = append(doctypes, dt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load all fields in a single batched query.
	allFields, err := s.loadAllFields(site)
	if err != nil {
		return nil, fmt.Errorf("loading all fields: %w", err)
	}

	// Group fields by parent doctype.
	for _, dt := range doctypes {
		dt.Fields = allFields[dt.Name]
	}

	return doctypes, nil
}

// loadAllFields fetches all fields for all doctypes in a single query.
func (s *Store) loadAllFields(site string) (map[string][]doctype.Field, error) {
	rows, err := s.DB.Query(`
		SELECT parent, fieldname, fieldtype, label, options, reqd, unique_constraint,
			default_value, hidden, read_only, bold, in_list_view, in_standard_filter,
			search_index, description, depends_on, mandatory_depends_on,
			constraints_json, renamed_from, COALESCE(linked_field,'') as linked_field, COALESCE(computed,'') as computed, idx
		FROM _kora_field
		WHERE site = ? OR site = ''
		ORDER BY parent, idx
	`, site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]doctype.Field)
	for rows.Next() {
		var parent string
		f := doctype.Field{}
		var reqd, unique, hidden, readOnly, bold, inListView, inStdFilter, searchIdx, idxVal int
		var constraintsJSON string

		err := rows.Scan(
			&parent, &f.Fieldname, &f.Fieldtype, &f.Label, &f.Options,
			&reqd, &unique, &f.Default, &hidden, &readOnly, &bold,
			&inListView, &inStdFilter, &searchIdx, &f.Description,
			&f.DependsOn, &f.MandatoryDependsOn, &constraintsJSON,
			&f.RenamedFrom, &f.LinkedField, &f.Computed, &idxVal,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning field: %w", err)
		}

		f.Reqd = reqd == 1
		f.Unique = unique == 1
		f.Hidden = hidden == 1
		f.ReadOnly = readOnly == 1
		f.Bold = bold == 1
		f.InListView = inListView == 1
		f.InStandardFilter = inStdFilter == 1
		f.SearchIndex = searchIdx == 1

		result[parent] = append(result[parent], f)
	}

	return result, rows.Err()
}


// boolToInt converts a bool to 0/1 for MySQL TINYINT columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}


// SaveRoles saves role definitions to _kora_role.
func (s *Store) SaveRoles(roles []*doctype.Role, site string) error {
	for _, role := range roles {
		upsertSQL := `INSERT INTO _kora_role (name, workspace_access, description, site)
			VALUES (?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"workspace_access", "description", "site"})
		_, err := s.DB.Exec(upsertSQL, role.Name, boolToInt(role.WorkspaceAccess), role.Description, site)
		if err != nil {
			return fmt.Errorf("saving role %s: %w", role.Name, err)
		}
	}
	return nil
}

// SaveRolesTx is like SaveRoles but uses an existing transaction.
func (s *Store) SaveRolesTx(tx *sql.Tx, roles []*doctype.Role, site string) error {
	for _, role := range roles {
		upsertSQL := `INSERT INTO _kora_role (name, workspace_access, description, site)
			VALUES (?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"workspace_access", "description", "site"})
		_, err := tx.Exec(upsertSQL, role.Name, boolToInt(role.WorkspaceAccess), role.Description, site)
		if err != nil {
			return fmt.Errorf("saving role %s: %w", role.Name, err)
		}
	}
	return nil
}

// SavePermissions saves permission definitions to _kora_permission.
func (s *Store) SavePermissions(permissions []*doctype.Permission, site string) error {
	for _, p := range permissions {
		name := fmt.Sprintf("%s.%s", p.Doctype, p.Role)
		upsertSQL := `INSERT INTO _kora_permission (name, doctype, role, can_read, can_write, can_create,
				can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner, site)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"can_read", "can_write", "can_create", "can_delete",
				"can_submit", "can_cancel", "can_amend", "can_export", "can_import", "can_report", "if_owner", "site"})
		_, err := s.DB.Exec(upsertSQL, name, p.Doctype, p.Role,
			boolToInt(p.Read), boolToInt(p.Write), boolToInt(p.Create),
			boolToInt(p.Delete), boolToInt(p.Submit), boolToInt(p.Cancel),
			boolToInt(p.Amend), boolToInt(p.Export), boolToInt(p.Import),
			boolToInt(p.Report), boolToInt(p.IfOwner), site,
		)
		if err != nil {
			return fmt.Errorf("saving permission %s: %w", name, err)
		}
	}
	return nil
}

// SavePermissionsTx is like SavePermissions but uses an existing transaction.
func (s *Store) SavePermissionsTx(tx *sql.Tx, permissions []*doctype.Permission, site string) error {
	for _, p := range permissions {
		name := fmt.Sprintf("%s.%s", p.Doctype, p.Role)
		upsertSQL := `INSERT INTO _kora_permission (name, doctype, role, can_read, can_write, can_create,
				can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner, site)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"can_read", "can_write", "can_create", "can_delete",
				"can_submit", "can_cancel", "can_amend", "can_export", "can_import", "can_report", "if_owner", "site"})
		_, err := tx.Exec(upsertSQL, name, p.Doctype, p.Role,
			boolToInt(p.Read), boolToInt(p.Write), boolToInt(p.Create),
			boolToInt(p.Delete), boolToInt(p.Submit), boolToInt(p.Cancel),
			boolToInt(p.Amend), boolToInt(p.Export), boolToInt(p.Import),
			boolToInt(p.Report), boolToInt(p.IfOwner), site,
		)
		if err != nil {
			return fmt.Errorf("saving permission %s: %w", name, err)
		}
	}
	return nil
}

// AutoCreatePermissionsForDoctype creates Administrator-only permissions for a
// newly created doctype. Other roles must be explicitly granted access via the
// Permissions panel. This follows the principle: explicit is better than implicit.
func (s *Store) AutoCreatePermissionsForDoctype(doctypeName string, site string) error {
	// Get all existing roles for this site.
	rows, err := s.DB.Query("SELECT name FROM _kora_role WHERE site = ? OR site = ''", site)
	if err != nil {
		return fmt.Errorf("querying roles: %w", err)
	}
	defer rows.Close()

	var roles []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		roles = append(roles, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Only Administrator gets auto-granted permissions. Other roles must be
	// explicitly granted access via the Permissions panel (explicit > implicit).
	for _, role := range roles {
		if role != "Administrator" {
			continue
		}
		name := fmt.Sprintf("%s.%s", doctypeName, role)
		upsertSQL := `INSERT INTO _kora_permission (name, doctype, role, can_read, can_write, can_create,
				can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner, site)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"can_read", "can_write", "can_create", "can_delete",
				"can_submit", "can_cancel", "can_amend", "can_export", "can_import", "can_report", "if_owner", "site"})
		_, err := s.DB.Exec(upsertSQL,
			name, doctypeName, role,
			true, true, true, // read, write, create
			true, true, true, // delete, submit, cancel
			true, true, true, true, // amend, export, import, report
			false, // if_owner
			site,
		)
		if err != nil {
			return fmt.Errorf("creating default permission for %s: %w", name, err)
		}
	}

	// Reload permissions into the in-memory registry so they take effect immediately.
	// The caller's registry will pick these up on the next request.
	slog.Info("auto-created default permissions for new doctype", "doctype", doctypeName, "roles", len(roles))
	return nil
}

// SaveWorkflows saves workflow definitions to _kora_workflow table.
func (s *Store) SaveWorkflows(workflows []*doctype.Workflow, site string) error {
	for _, wf := range workflows {
		configJSON, _ := json.Marshal(wf)

		upsertSQL := `INSERT INTO _kora_workflow (name, document_type, is_active, workflow_state_field, config_json, site)
			VALUES (?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"is_active", "config_json", "site"})
		_, err := s.DB.Exec(upsertSQL, wf.Name, wf.DocumentType, boolToInt(wf.IsActive), wf.WorkflowStateField, string(configJSON), site)
		if err != nil {
			return fmt.Errorf("saving workflow %s: %w", wf.Name, err)
		}

		// Save states.
		for i, state := range wf.States {
			stateName := fmt.Sprintf("%s.%s", wf.Name, state.State)
			upsertSQL := `INSERT INTO _kora_workflow_state (name, workflow, state, doc_status, allow_edit, style, idx)
				VALUES (?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
				[]string{"name"}, []string{"doc_status", "allow_edit", "style"})
			_, err := s.DB.Exec(upsertSQL, stateName, wf.Name, state.State, state.DocStatus, state.AllowEdit, state.Style, i)
			if err != nil {
				return fmt.Errorf("saving workflow state %s: %w", stateName, err)
			}
		}

		// Save transitions.
		for i, t := range wf.Transitions {
			transName := fmt.Sprintf("%s.%s", wf.Name, t.Action)
			requireFields := strings.Join(t.RequireFields, ",")
			upsertSQL := `INSERT INTO _kora_workflow_transition (name, workflow, action, from_state, to_state, allowed, condition_expr, require_fields, idx)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
				[]string{"name"}, []string{"from_state", "to_state", "allowed"})
			_, err := s.DB.Exec(upsertSQL, transName, wf.Name, t.Action, t.From, t.To, t.Allowed, t.Condition, requireFields, i)
			if err != nil {
				return fmt.Errorf("saving workflow transition %s: %w", transName, err)
			}
		}
	}
	return nil
}

// SaveWorkflowsTx is like SaveWorkflows but uses an existing transaction.
func (s *Store) SaveWorkflowsTx(tx *sql.Tx, workflows []*doctype.Workflow, site string) error {
	for _, wf := range workflows {
		configJSON, _ := json.Marshal(wf)

		upsertSQL := `INSERT INTO _kora_workflow (name, document_type, is_active, workflow_state_field, config_json, site)
			VALUES (?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"is_active", "config_json", "site"})
		_, err := tx.Exec(upsertSQL, wf.Name, wf.DocumentType, boolToInt(wf.IsActive), wf.WorkflowStateField, string(configJSON), site)
		if err != nil {
			return fmt.Errorf("saving workflow %s: %w", wf.Name, err)
		}

		// Save states.
		for i, state := range wf.States {
			stateName := fmt.Sprintf("%s.%s", wf.Name, state.State)
			upsertSQL := `INSERT INTO _kora_workflow_state (name, workflow, state, doc_status, allow_edit, style, idx)
				VALUES (?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
				[]string{"name"}, []string{"doc_status", "allow_edit", "style"})
			_, err := tx.Exec(upsertSQL, stateName, wf.Name, state.State, state.DocStatus, state.AllowEdit, state.Style, i)
			if err != nil {
				return fmt.Errorf("saving workflow state %s: %w", stateName, err)
			}
		}

		// Save transitions.
		for i, t := range wf.Transitions {
			transName := fmt.Sprintf("%s.%s", wf.Name, t.Action)
			requireFields := strings.Join(t.RequireFields, ",")
			upsertSQL := `INSERT INTO _kora_workflow_transition (name, workflow, action, from_state, to_state, allowed, condition_expr, require_fields, idx)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
				[]string{"name"}, []string{"from_state", "to_state", "allowed"})
			_, err := tx.Exec(upsertSQL, transName, wf.Name, t.Action, t.From, t.To, t.Allowed, t.Condition, requireFields, i)
			if err != nil {
				return fmt.Errorf("saving workflow transition %s: %w", transName, err)
			}
		}
	}
	return nil
}

// LoadRoles loads all roles from _kora_role.
func (s *Store) LoadRoles(site string) ([]*doctype.Role, error) {
	rows, err := s.DB.Query("SELECT name, workspace_access, description FROM _kora_role WHERE site = ? OR site = '' ORDER BY name", site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var roles []*doctype.Role
	for rows.Next() {
		r := &doctype.Role{}
		var workspaceAccess int
		if err := rows.Scan(&r.Name, &workspaceAccess, &r.Description); err != nil {
			return nil, err
		}
		r.WorkspaceAccess = workspaceAccess == 1
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// LoadPermissions loads all permissions from _kora_permission.
func (s *Store) LoadPermissions(site string) ([]*doctype.Permission, error) {
	rows, err := s.DB.Query(`
		SELECT doctype, role, can_read, can_write, can_create, can_delete,
			can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner
		FROM _kora_permission
		WHERE site = ? OR site = ''
		ORDER BY doctype, role
	`, site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var perms []*doctype.Permission
	for rows.Next() {
		p := &doctype.Permission{}
		var read, write, create, del, submit, cancel, amend, export, imp, report, ifOwner int
		if err := rows.Scan(&p.Doctype, &p.Role, &read, &write, &create, &del,
			&submit, &cancel, &amend, &export, &imp, &report, &ifOwner); err != nil {
			return nil, err
		}
		p.Read = read == 1
		p.Write = write == 1
		p.Create = create == 1
		p.Delete = del == 1
		p.Submit = submit == 1
		p.Cancel = cancel == 1
		p.Amend = amend == 1
		p.Export = export == 1
		p.Import = imp == 1
		p.Report = report == 1
		p.IfOwner = ifOwner == 1
		perms = append(perms, p)
	}
	return perms, rows.Err()
}

// CreateConfigVersion saves a snapshot of the full config for version tracking.
// The snapshot includes doctypes, roles, permissions, and workflows.
// Returns the version ID, version number, and any error.
func (s *Store) CreateConfigVersion(siteName, createdBy, label, status string, snapshot *doctype.ConfigSnapshot) (string, int, error) {
	if snapshot == nil {
		return "", 0, fmt.Errorf("snapshot is required")
	}
	var currentVersion int
	s.DB.QueryRow("SELECT COALESCE(MAX(version), 0) FROM _kora_config_version WHERE site = ?", siteName).Scan(&currentVersion)
	newVersion := currentVersion + 1

	// If activating, deactivate all other versions first.
	if status == "Active" {
		s.DB.Exec("UPDATE _kora_config_version SET status = 'Superseded' WHERE site = ? AND status = 'Active'", siteName)
	}
	// Serialize as canonical s-expression.
	configSExpr := doctype.ToSExpr(snapshot)
	h := sha256.Sum256([]byte(configSExpr))
	configHash := hex.EncodeToString(h[:])

	// Compute diff against previous version if one exists.
	// Handles both old format (JSON array of doctypes) and new format (ConfigSnapshot).
	var changelog any
	var changeList any
	var prevConfigRaw string
	s.DB.QueryRow("SELECT config FROM _kora_config_version WHERE site = ? AND version = ?", siteName, currentVersion).Scan(&prevConfigRaw)

	if prevConfigRaw != "" {
		prevSnapshot, parseErr := doctype.ParseConfig(prevConfigRaw)
		if parseErr == nil {
			// Compute the full-snapshot diff (includes non-doctype sections).
			fullDiff := doctype.DiffFullSnapshots(prevSnapshot, snapshot)
			fullDiff.Doctypes.FromVersion = currentVersion
			fullDiff.Doctypes.ToVersion = newVersion
			changeListBytes, _ := json.Marshal(fullDiff)
			changeList = string(changeListBytes)

			// Also keep backward-compat changelog (doctype-only diff).
			doctypeDiff := doctype.DiffConfigs(prevSnapshot.DocTypes, snapshot.DocTypes)
			doctypeDiff.FromVersion = currentVersion
			doctypeDiff.ToVersion = newVersion
			changelogBytes, _ := json.Marshal(doctypeDiff)
			changelog = string(changelogBytes)
		}
	}

	// Look up base_version_id from the current active version (for staleness tracking).
	var baseVersionID string
	if status == "Draft" {
		s.DB.QueryRow("SELECT id FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1", siteName).Scan(&baseVersionID)
	}

	// Get min_kora_version from the snapshot, or use empty string.
	minKoraVersion := snapshot.MinKoraVersion

	versionID := fmt.Sprintf("cv-%s-%d", siteName, newVersion)
	_, err := s.DB.Exec(
		`INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, changelog, status, config, change_list, config_hash, base_version_id, min_kora_version)
		 VALUES (?, ?, ?, `+s.Dialect.NowTimestamp()+`, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		versionID, siteName, newVersion, createdBy, label, changelog, status, configSExpr,
		changeList, configHash, baseVersionID, minKoraVersion,
	)
	if err != nil {
		return "", 0, fmt.Errorf("creating config version: %w", err)
	}

	slog.Debug("created config version", "id", versionID, "version", newVersion, "status", status)
	return versionID, newVersion, nil
}

// CollectSnapshot reads all live config state and builds a ConfigSnapshot for versioning.
// Call before creating a version to capture the complete current state.
// Doctypes are sorted by name for deterministic snapshot ordering (fixes non-deterministic map iteration).
func (s *Store) CollectSnapshot(reg *doctype.Registry, site string) (*doctype.ConfigSnapshot, error) {
	doctypes := make([]*doctype.DocType, 0)
	for _, name := range reg.Names() {
		if dt := reg.Get(name); dt != nil {
			doctypes = append(doctypes, dt)
		}
	}
	sort.Slice(doctypes, func(i, j int) bool { return doctypes[i].Name < doctypes[j].Name })
	roles, _ := s.LoadRoles(site)
	permissions, _ := s.LoadPermissions(site)
	workflows, _ := s.LoadWorkflows(site)
	analyticsMetrics, _ := s.LoadAnalyticsMetrics(site)
	scripts, _ := s.LoadScriptSnapshots(site)
	return &doctype.ConfigSnapshot{
		DocTypes:         doctypes,
		Roles:            roles,
		Permissions:      permissions,
		Workflows:        workflows,
		AnalyticsMetrics: analyticsMetrics,
		Scripts:          scripts,
	}, nil
}

// LoadAnalyticsMetrics loads all custom analytics metric definitions.
func (s *Store) LoadAnalyticsMetrics(site string) ([]*doctype.AnalyticsMetricConfig, error) {
	rows, err := s.DB.Query("SELECT name, label, type, doctype, field_name, link_field, group_by_field FROM _kora_analytics_metric WHERE site = ? OR site = ''", site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metrics []*doctype.AnalyticsMetricConfig
	for rows.Next() {
		var m doctype.AnalyticsMetricConfig
		rows.Scan(&m.Name, &m.Label, &m.Type, &m.DocType, &m.FieldName, &m.LinkField, &m.GroupByField)
		m.AutoGenerated = false
		metrics = append(metrics, &m)
	}
	return metrics, rows.Err()
}

// SaveAnalyticsMetrics saves custom analytics metric definitions from a snapshot.
func (s *Store) SaveAnalyticsMetrics(metrics []*doctype.AnalyticsMetricConfig, site string) error {
	for _, m := range metrics {
		if m == nil {
			continue
		}
		upsertSQL := `INSERT INTO _kora_analytics_metric (name, label, type, doctype, field_name, link_field, group_by_field, site)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			` + s.Dialect.UpsertClause([]string{"name"}, []string{"label", "type", "doctype", "field_name", "link_field", "group_by_field", "site"})
		_, err := s.DB.Exec(upsertSQL, m.Name, m.Label, m.Type, m.DocType, m.FieldName, m.LinkField, m.GroupByField, site)
		if err != nil {
			return fmt.Errorf("saving analytics metric %s: %w", m.Name, err)
		}
	}
	return nil
}

// SaveAnalyticsMetricsTx is like SaveAnalyticsMetrics but uses an existing transaction.
func (s *Store) SaveAnalyticsMetricsTx(tx *sql.Tx, metrics []*doctype.AnalyticsMetricConfig, site string) error {
	for _, m := range metrics {
		if m == nil {
			continue
		}
		upsertSQL := `INSERT INTO _kora_analytics_metric (name, label, type, doctype, field_name, link_field, group_by_field, site)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			` + s.Dialect.UpsertClause([]string{"name"}, []string{"label", "type", "doctype", "field_name", "link_field", "group_by_field", "site"})
		_, err := tx.Exec(upsertSQL, m.Name, m.Label, m.Type, m.DocType, m.FieldName, m.LinkField, m.GroupByField, site)
		if err != nil {
			return fmt.Errorf("saving analytics metric %s: %w", m.Name, err)
		}
	}
	return nil
}

// LoadScriptSnapshots loads all active scripts as snapshots for versioning.
// Script bodies are hashed (SHA-256) rather than stored inline to avoid DB bloat.
func (s *Store) LoadScriptSnapshots(site string) ([]*doctype.ScriptSnapshot, error) {
	rows, err := s.DB.Query(`SELECT name, script_type, doctype, event, method_path, workflow_action, schedule,
		priority, is_active, run_as, timeout_ms, script FROM _kora_script WHERE is_active = 1 AND (site = ? OR site = '')`, site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var scripts []*doctype.ScriptSnapshot
	for rows.Next() {
		var ss doctype.ScriptSnapshot
		var script string
		if err := rows.Scan(&ss.Name, &ss.ScriptType, &ss.DocType, &ss.Event, &ss.MethodPath,
			&ss.WorkflowAction, &ss.Schedule, &ss.Priority, &ss.IsActive, &ss.RunAs,
			&ss.TimeoutMs, &script); err != nil {
			return nil, err
		}
		h := sha256.Sum256([]byte(script))
		ss.ScriptHash = hex.EncodeToString(h[:])
		scripts = append(scripts, &ss)
	}
	return scripts, rows.Err()
}

// LoadWorkflows loads all workflows from the database.
func (s *Store) LoadWorkflows(site string) ([]*doctype.Workflow, error) {
	rows, err := s.DB.Query("SELECT config_json FROM _kora_workflow WHERE is_active = 1 AND (site = ? OR site = '')", site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workflows []*doctype.Workflow
	for rows.Next() {
		var configJSON string
		if err := rows.Scan(&configJSON); err != nil {
			return nil, err
		}
		wf := &doctype.Workflow{}
		if err := json.Unmarshal([]byte(configJSON), wf); err != nil {
			return nil, fmt.Errorf("unmarshaling workflow: %w", err)
		}
		workflows = append(workflows, wf)
	}
	return workflows, rows.Err()
}

// ActivateSnapshot runs the full activation in a single transaction.
// 1. Writes all config rows (doctypes, roles, permissions, workflows, metrics)
// 2. Applies DDL (caller must apply DDL after the tx, or use tx.Exec for SQLite)
// 3. Rebuilds registry from the snapshot in memory
// On any failure, returns an error (caller should roll back the transaction).
func (s *Store) ActivateSnapshot(tx *sql.Tx, snapshot *doctype.ConfigSnapshot, reg *doctype.Registry, siteName string, dialect db.Dialect) error {
	// Step 1: Save all doctypes using the transaction.
	for _, dt := range snapshot.DocTypes {
		if err := s.SaveDocTypeTx(tx, dt, siteName); err != nil {
			return fmt.Errorf("saving doctype %s during activation: %w", dt.Name, err)
		}
	}

	// Step 2: Save roles, permissions, workflows, and analytics metrics.
	if len(snapshot.Roles) > 0 {
		if err := s.SaveRolesTx(tx, snapshot.Roles, siteName); err != nil {
			return fmt.Errorf("saving roles during activation: %w", err)
		}
	}
	if len(snapshot.Permissions) > 0 {
		if err := s.SavePermissionsTx(tx, snapshot.Permissions, siteName); err != nil {
			return fmt.Errorf("saving permissions during activation: %w", err)
		}
	}
	if len(snapshot.Workflows) > 0 {
		if err := s.SaveWorkflowsTx(tx, snapshot.Workflows, siteName); err != nil {
			return fmt.Errorf("saving workflows during activation: %w", err)
		}
	}
	if len(snapshot.AnalyticsMetrics) > 0 {
		if err := s.SaveAnalyticsMetricsTx(tx, snapshot.AnalyticsMetrics, siteName); err != nil {
			return fmt.Errorf("saving analytics metrics during activation: %w", err)
		}
	}

	// Step 3: Rebuild registry from snapshot (in-memory, no DB needed).
	reg.LoadFull(snapshot.DocTypes, snapshot.Roles, snapshot.Permissions)
	reg.Workflows.LoadFromDB(snapshot.Workflows)

	return nil
}

// ApplyDDLTx executes DDL statements using the given transaction.
// Unlike dialect.ExecuteBatch (which uses *sql.DB), this applies DDL within
// a caller-managed transaction, which is important for SQLite/LibSQL atomic
// DDL but won't prevent MySQL DDL auto-commit.
func ApplyDDLTx(tx *sql.Tx, statements []string) error {
	for _, stmt := range statements {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("executing DDL: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

// MigrateLegacyConfigs converts any JSON-format config versions to s-expression format.
// This is a lazy migration: existing JSON configs are converted in-place as they are discovered.
// On the first call, it scans all versions with JSON-format configs and converts them.
// Run at startup or on first access to config versions.
func (s *Store) MigrateLegacyConfigs(site string) (int, error) {
	rows, err := s.DB.Query(
		"SELECT id, config FROM _kora_config_version WHERE site = ? AND config != '' AND config IS NOT NULL",
		site,
	)
	if err != nil {
		return 0, fmt.Errorf("querying config versions for migration: %w", err)
	}
	defer rows.Close()

	var toMigrate []struct {
		id     string
		config string
	}

	for rows.Next() {
		var id, config string
		if err := rows.Scan(&id, &config); err != nil {
			return 0, fmt.Errorf("scanning config version: %w", err)
		}
		// Only migrate JSON-format configs (starts with { or [).
		if doctype.IsJSONConfig(config) {
			toMigrate = append(toMigrate, struct {
				id     string
				config string
			}{id, config})
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	migrated := 0
	for _, m := range toMigrate {
		snapshot, err := doctype.ParseSnapshot(m.config)
		if err != nil {
			slog.Warn("cannot migrate config version, skipping", "id", m.id, "error", err)
			continue
		}
		sexpr := doctype.ToSExpr(snapshot)
		_, err = s.DB.Exec("UPDATE _kora_config_version SET config = ? WHERE id = ?", sexpr, m.id)
		if err != nil {
			return migrated, fmt.Errorf("updating migrated config for %s: %w", m.id, err)
		}
		migrated++
	}

	if migrated > 0 {
		slog.Info("migrated legacy config versions to s-expression", "site", site, "count", migrated)
	}
	return migrated, nil
}

// MigrateAllLegacyConfigs migrates legacy JSON configs for all sites.
func (s *Store) MigrateAllLegacyConfigs() (int, error) {
	rows, err := s.DB.Query("SELECT DISTINCT site FROM _kora_config_version WHERE config != '' AND config IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("querying distinct sites for config migration: %w", err)
	}
	defer rows.Close()

	total := 0
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			return 0, err
		}
		n, err := s.MigrateLegacyConfigs(site)
		if err != nil {
			slog.Warn("error migrating site", "site", site, "error", err)
			continue
		}
		total += n
	}
	return total, rows.Err()
}
