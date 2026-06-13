// Package configstore manages reading and writing DocType configuration
// to/from the database (_kora_doctype and _kora_field tables).
package configstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yourorg/kora/doctype"
)

// Store persists DocType configurations to the database.
type Store struct {
	DB *sql.DB
}

// NewStore creates a new config store.
func NewStore(db *sql.DB) *Store {
	return &Store{DB: db}
}

// SaveDocType inserts or updates a DocType and its fields in the database.
// Uses a transaction for atomicity. Existing fields are diffed against new fields:
// only removed fields are deleted, only new fields are inserted, changed fields are updated.
func (s *Store) SaveDocType(dt *doctype.DocType) error {
	dbTx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer dbTx.Rollback()

	configJSON, err := json.Marshal(dt)
	if err != nil {
		return fmt.Errorf("marshaling doctype: %w", err)
	}

	// Upsert the DocType record.
	_, err = dbTx.Exec(`
		INSERT INTO _kora_doctype (name, module, is_submittable, is_child_table, is_single,
			track_changes, title_field, search_fields, sort_field, sort_order, description, config_json, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON DUPLICATE KEY UPDATE
			module = VALUES(module),
			is_submittable = VALUES(is_submittable),
			is_child_table = VALUES(is_child_table),
			is_single = VALUES(is_single),
			track_changes = VALUES(track_changes),
			title_field = VALUES(title_field),
			search_fields = VALUES(search_fields),
			sort_field = VALUES(sort_field),
			sort_order = VALUES(sort_order),
			description = VALUES(description),
			config_json = VALUES(config_json),
			version = version + 1
	`,
		dt.Name, dt.Module, boolToInt(dt.IsSubmittable), boolToInt(dt.IsChildTable),
		boolToInt(dt.IsSingle), boolToInt(dt.TrackChanges),
		dt.TitleField, dt.SearchFields, dt.SortField, dt.SortOrder,
		dt.Description, string(configJSON),
	)
	if err != nil {
		return fmt.Errorf("saving doctype %s: %w", dt.Name, err)
	}

	// Load existing fields for diff-based merge.
	existingFields, err := s.loadFieldsMap(dt.Name, dbTx)
	if err != nil {
		return fmt.Errorf("loading existing fields for %s: %w", dt.Name, err)
	}

	// Build set of new field names.
	newFieldNames := make(map[string]bool)
	for _, field := range dt.Fields {
		newFieldNames[field.Fieldname] = true
	}

	// Delete fields that are in the DB but NOT in the new config.
	var toDelete []string
	for name := range existingFields {
		if !newFieldNames[name] {
			toDelete = append(toDelete, name)
		}
	}
	for _, name := range toDelete {
		if _, err := dbTx.Exec("DELETE FROM _kora_field WHERE parent = ? AND fieldname = ?", dt.Name, name); err != nil {
			return fmt.Errorf("deleting removed field %s.%s: %w", dt.Name, name, err)
		}
		slog.Info("removed field from config import", "doctype", dt.Name, "field", name)
	}

	// Insert new fields and update existing ones.
	for i, field := range dt.Fields {
		constraintsJSON, _ := json.Marshal(field.Constraints)

		if _, exists := existingFields[field.Fieldname]; exists {
			// UPDATE existing field.
			_, err = dbTx.Exec(`
				UPDATE _kora_field SET
					fieldtype = ?, label = ?, options = ?, reqd = ?, unique_constraint = ?,
					default_value = ?, hidden = ?, read_only = ?, bold = ?,
					in_list_view = ?, in_standard_filter = ?, search_index = ?, description = ?,
					depends_on = ?, mandatory_depends_on = ?, constraints_json = ?,
					renamed_from = ?, linked_field = ?, computed = ?, idx = ?
				WHERE parent = ? AND fieldname = ?
			`,
				field.Fieldtype, field.Label, field.Options,
				boolToInt(field.Reqd), boolToInt(field.Unique), field.Default,
				boolToInt(field.Hidden), boolToInt(field.ReadOnly), boolToInt(field.Bold),
				boolToInt(field.InListView), boolToInt(field.InStandardFilter),
				boolToInt(field.SearchIndex), field.Description,
				field.DependsOn, field.MandatoryDependsOn, string(constraintsJSON),
				field.RenamedFrom, field.LinkedField, field.Computed, i,
				dt.Name, field.Fieldname,
			)
			if err != nil {
				return fmt.Errorf("updating field %s.%s: %w", dt.Name, field.Fieldname, err)
			}
		} else {
			// INSERT new field.
			_, err = dbTx.Exec(`
				INSERT INTO _kora_field (name, parent, fieldname, fieldtype, label, options,
					reqd, unique_constraint, default_value, hidden, read_only, bold,
					in_list_view, in_standard_filter, search_index, description,
					depends_on, mandatory_depends_on, constraints_json, renamed_from, linked_field, computed, idx)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				fmt.Sprintf("%s.%s", dt.Name, field.Fieldname),
				dt.Name, field.Fieldname, field.Fieldtype, field.Label, field.Options,
				boolToInt(field.Reqd), boolToInt(field.Unique), field.Default,
				boolToInt(field.Hidden), boolToInt(field.ReadOnly), boolToInt(field.Bold),
				boolToInt(field.InListView), boolToInt(field.InStandardFilter),
				boolToInt(field.SearchIndex), field.Description,
				field.DependsOn, field.MandatoryDependsOn, string(constraintsJSON),
				field.RenamedFrom, field.LinkedField, field.Computed, i,
			)
			if err != nil {
				return fmt.Errorf("inserting field %s.%s: %w", dt.Name, field.Fieldname, err)
			}
		}
	}

	if err := dbTx.Commit(); err != nil {
		return fmt.Errorf("committing doctype %s: %w", dt.Name, err)
	}

	slog.Debug("saved doctype config", "name", dt.Name, "fields", len(dt.Fields),
		"deleted", len(toDelete), "new", len(dt.Fields)-len(existingFields)+len(toDelete))
	return nil
}

// loadFieldsMap returns existing field definitions keyed by fieldname.
func (s *Store) loadFieldsMap(parent string, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.Query("SELECT fieldname FROM _kora_field WHERE parent = ?", parent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		result[name] = true
	}
	return result, rows.Err()
}

// LoadAll loads all DocTypes from the database into the registry.
// Uses two batched queries instead of N+1 (one for doctypes, one for all fields).
func (s *Store) LoadAll() ([]*doctype.DocType, error) {
	rows, err := s.DB.Query(`
		SELECT name, module, is_submittable, is_child_table, is_single,
			track_changes, title_field, search_fields, sort_field, sort_order, description
		FROM _kora_doctype
		ORDER BY name
	`)
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
	allFields, err := s.loadAllFields()
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
func (s *Store) loadAllFields() (map[string][]doctype.Field, error) {
	rows, err := s.DB.Query(`
		SELECT parent, fieldname, fieldtype, label, options, reqd, unique_constraint,
			default_value, hidden, read_only, bold, in_list_view, in_standard_filter,
			search_index, description, depends_on, mandatory_depends_on,
			constraints_json, renamed_from, COALESCE(linked_field,'') as linked_field, COALESCE(computed,'') as computed, idx
		FROM _kora_field
		ORDER BY parent, idx
	`)
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

func (s *Store) loadFields(parent string) ([]doctype.Field, error) {
	rows, err := s.DB.Query(`
		SELECT fieldname, fieldtype, label, options, reqd, unique_constraint,
			default_value, hidden, read_only, bold, in_list_view, in_standard_filter,
			search_index, description, depends_on, mandatory_depends_on,
			constraints_json, renamed_from, COALESCE(linked_field,'') as linked_field, COALESCE(computed,'') as computed, idx
		FROM _kora_field
		WHERE parent = ?
		ORDER BY idx
	`, parent)
	if err != nil {
		return nil, fmt.Errorf("querying fields: %w", err)
	}
	defer rows.Close()

	var fields []doctype.Field
	for rows.Next() {
		f := doctype.Field{}
		var reqd, unique, hidden, readOnly, bold, inListView, inStdFilter, searchIdx, idxVal int
		var constraintsJSON string

		err := rows.Scan(
			&f.Fieldname, &f.Fieldtype, &f.Label, &f.Options,
			&reqd, &unique, &f.Default, &hidden, &readOnly, &bold,
			&inListView, &inStdFilter, &searchIdx,
			&f.Description, &f.DependsOn, &f.MandatoryDependsOn,
			&constraintsJSON, &f.RenamedFrom, &f.LinkedField, &f.Computed, &idxVal,
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

		// Parse constraints.
		if constraintsJSON != "" && constraintsJSON != "null" {
			json.Unmarshal([]byte(constraintsJSON), &f.Constraints)
		}

		fields = append(fields, f)
	}

	return fields, rows.Err()
}

// boolToInt converts a bool to 0/1 for MySQL TINYINT columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetTargetFields returns the field names used by Link fields in a doctype,
// for use when resolving "exists" constraints.
func GetTargetFields(dt *doctype.DocType) []string {
	var fields []string
	for _, f := range dt.Fields {
		if f.Fieldtype == "Link" {
			fields = append(fields, f.Options)
		}
	}
	return fields
}

// SaveRoles saves role definitions to _kora_role.
func (s *Store) SaveRoles(roles []*doctype.Role) error {
	// Ensure table exists.
	s.DB.Exec(`CREATE TABLE IF NOT EXISTS _kora_role (
		name VARCHAR(140) PRIMARY KEY,
		workspace_access TINYINT(1) NOT NULL DEFAULT 1,
		description TEXT
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)

	for _, role := range roles {
		_, err := s.DB.Exec(`
			INSERT INTO _kora_role (name, workspace_access, description)
			VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE workspace_access = VALUES(workspace_access), description = VALUES(description)
		`, role.Name, boolToInt(role.WorkspaceAccess), role.Description)
		if err != nil {
			return fmt.Errorf("saving role %s: %w", role.Name, err)
		}
	}
	return nil
}

// SavePermissions saves permission definitions to _kora_permission.
func (s *Store) SavePermissions(permissions []*doctype.Permission) error {
	// Ensure table exists.
	s.DB.Exec(`CREATE TABLE IF NOT EXISTS _kora_permission (
		name VARCHAR(140) PRIMARY KEY,
		doctype VARCHAR(140) NOT NULL, role VARCHAR(140) NOT NULL,
		can_read TINYINT(1) NOT NULL DEFAULT 0, can_write TINYINT(1) NOT NULL DEFAULT 0,
		can_create TINYINT(1) NOT NULL DEFAULT 0, can_delete TINYINT(1) NOT NULL DEFAULT 0,
		can_submit TINYINT(1) NOT NULL DEFAULT 0, can_cancel TINYINT(1) NOT NULL DEFAULT 0,
		can_amend TINYINT(1) NOT NULL DEFAULT 0, can_export TINYINT(1) NOT NULL DEFAULT 0,
		can_import TINYINT(1) NOT NULL DEFAULT 0, can_report TINYINT(1) NOT NULL DEFAULT 0,
		if_owner TINYINT(1) NOT NULL DEFAULT 0,
		UNIQUE KEY idx_doctype_role (doctype, role)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)

	for _, p := range permissions {
		name := fmt.Sprintf("%s.%s", p.Doctype, p.Role)
		_, err := s.DB.Exec(`
			INSERT INTO _kora_permission (name, doctype, role, can_read, can_write, can_create,
				can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				can_read = VALUES(can_read), can_write = VALUES(can_write),
				can_create = VALUES(can_create), can_delete = VALUES(can_delete),
				can_submit = VALUES(can_submit), can_cancel = VALUES(can_cancel),
				can_amend = VALUES(can_amend), can_export = VALUES(can_export),
				can_import = VALUES(can_import), can_report = VALUES(can_report),
				if_owner = VALUES(if_owner)
		`, name, p.Doctype, p.Role,
			boolToInt(p.Read), boolToInt(p.Write), boolToInt(p.Create),
			boolToInt(p.Delete), boolToInt(p.Submit), boolToInt(p.Cancel),
			boolToInt(p.Amend), boolToInt(p.Export), boolToInt(p.Import),
			boolToInt(p.Report), boolToInt(p.IfOwner),
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
func (s *Store) AutoCreatePermissionsForDoctype(doctypeName string) error {
	// Ensure _kora_permission table exists.
	s.DB.Exec(`CREATE TABLE IF NOT EXISTS _kora_permission (
		name VARCHAR(140) PRIMARY KEY,
		doctype VARCHAR(140) NOT NULL, role VARCHAR(140) NOT NULL,
		can_read TINYINT(1) NOT NULL DEFAULT 0, can_write TINYINT(1) NOT NULL DEFAULT 0,
		can_create TINYINT(1) NOT NULL DEFAULT 0, can_delete TINYINT(1) NOT NULL DEFAULT 0,
		can_submit TINYINT(1) NOT NULL DEFAULT 0, can_cancel TINYINT(1) NOT NULL DEFAULT 0,
		can_amend TINYINT(1) NOT NULL DEFAULT 0, can_export TINYINT(1) NOT NULL DEFAULT 0,
		can_import TINYINT(1) NOT NULL DEFAULT 0, can_report TINYINT(1) NOT NULL DEFAULT 0,
		if_owner TINYINT(1) NOT NULL DEFAULT 0,
		UNIQUE KEY idx_doctype_role (doctype, role)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)

	// Get all existing roles.
	rows, err := s.DB.Query("SELECT name FROM _kora_role")
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
		_, err := s.DB.Exec(`
			INSERT INTO _kora_permission (name, doctype, role, can_read, can_write, can_create,
				can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				can_read = VALUES(can_read), can_write = VALUES(can_write),
				can_create = VALUES(can_create), can_delete = VALUES(can_delete),
				can_submit = VALUES(can_submit), can_cancel = VALUES(can_cancel),
				can_amend = VALUES(can_amend), can_export = VALUES(can_export),
				can_import = VALUES(can_import), can_report = VALUES(can_report),
				if_owner = VALUES(if_owner)
		`,
			name, doctypeName, role,
			true, true, true, // read, write, create
			true, true, true, // delete, submit, cancel
			true, true, true, true, // amend, export, import, report
			false, // if_owner
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
func (s *Store) SaveWorkflows(workflows []*doctype.Workflow) error {
	// Ensure _kora_workflow table exists.
	_, err := s.DB.Exec(`
		CREATE TABLE IF NOT EXISTS _kora_workflow (
			name VARCHAR(140) PRIMARY KEY,
			document_type VARCHAR(140) NOT NULL,
			is_active TINYINT(1) NOT NULL DEFAULT 1,
			workflow_state_field VARCHAR(140) NOT NULL DEFAULT 'status',
			config_json JSON,
			INDEX idx_doctype (document_type)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
	`)
	if err != nil {
		return fmt.Errorf("creating _kora_workflow table: %w", err)
	}

	// Also ensure workflow state and transition tables.
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS _kora_workflow_state (
			name VARCHAR(140) PRIMARY KEY,
			workflow VARCHAR(140) NOT NULL,
			state VARCHAR(140) NOT NULL,
			doc_status INT NOT NULL DEFAULT 0,
			allow_edit VARCHAR(140) NOT NULL DEFAULT '',
			style VARCHAR(20) NOT NULL DEFAULT 'default',
			idx INT NOT NULL DEFAULT 0,
			INDEX idx_workflow (workflow)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_workflow_transition (
			name VARCHAR(140) PRIMARY KEY,
			workflow VARCHAR(140) NOT NULL,
			action VARCHAR(140) NOT NULL,
			from_state VARCHAR(140) NOT NULL,
			to_state VARCHAR(140) NOT NULL,
			allowed VARCHAR(255) NOT NULL DEFAULT '',
			condition_expr TEXT,
			require_fields TEXT,
			idx INT NOT NULL DEFAULT 0,
			INDEX idx_workflow (workflow)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	} {
		if _, err := s.DB.Exec(ddl); err != nil {
			return fmt.Errorf("creating workflow system table: %w", err)
		}
	}

	for _, wf := range workflows {
		configJSON, _ := json.Marshal(wf)

		_, err := s.DB.Exec(`
			INSERT INTO _kora_workflow (name, document_type, is_active, workflow_state_field, config_json)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				is_active = VALUES(is_active), config_json = VALUES(config_json)
		`, wf.Name, wf.DocumentType, boolToInt(wf.IsActive), wf.WorkflowStateField, string(configJSON))
		if err != nil {
			return fmt.Errorf("saving workflow %s: %w", wf.Name, err)
		}

		// Save states.
		for i, state := range wf.States {
			stateName := fmt.Sprintf("%s.%s", wf.Name, state.State)
			_, err := s.DB.Exec(`
				INSERT INTO _kora_workflow_state (name, workflow, state, doc_status, allow_edit, style, idx)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE doc_status = VALUES(doc_status), allow_edit = VALUES(allow_edit), style = VALUES(style)
			`, stateName, wf.Name, state.State, state.DocStatus, state.AllowEdit, state.Style, i)
			if err != nil {
				return fmt.Errorf("saving workflow state %s: %w", stateName, err)
			}
		}

		// Save transitions.
		for i, t := range wf.Transitions {
			transName := fmt.Sprintf("%s.%s", wf.Name, t.Action)
			requireFields := strings.Join(t.RequireFields, ",")
			_, err := s.DB.Exec(`
				INSERT INTO _kora_workflow_transition (name, workflow, action, from_state, to_state, allowed, condition_expr, require_fields, idx)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE from_state = VALUES(from_state), to_state = VALUES(to_state), allowed = VALUES(allowed)
			`, transName, wf.Name, t.Action, t.From, t.To, t.Allowed, t.Condition, requireFields, i)
			if err != nil {
				return fmt.Errorf("saving workflow transition %s: %w", transName, err)
			}
		}
	}
	return nil
}

// LoadRoles loads all roles from _kora_role.
func (s *Store) LoadRoles() ([]*doctype.Role, error) {
	rows, err := s.DB.Query("SELECT name, workspace_access, description FROM _kora_role ORDER BY name")
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
func (s *Store) LoadPermissions() ([]*doctype.Permission, error) {
	rows, err := s.DB.Query(`
		SELECT doctype, role, can_read, can_write, can_create, can_delete,
			can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner
		FROM _kora_permission ORDER BY doctype, role
	`)
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
// Returns the version ID, version number, and any error.
func (s *Store) CreateConfigVersion(siteName, createdBy, label, status string, doctypes []*doctype.DocType) (string, int, error) {
	var currentVersion int
	s.DB.QueryRow("SELECT COALESCE(MAX(version), 0) FROM _kora_config_version WHERE site = ?", siteName).Scan(&currentVersion)
	newVersion := currentVersion + 1

	// If activating, deactivate all other versions first.
	if status == "Active" {
		s.DB.Exec("UPDATE _kora_config_version SET status = 'Superseded' WHERE site = ? AND status = 'Active'", siteName)
	}

	// Serialize config snapshot as JSON.
	configJSON, err := json.Marshal(doctypes)
	if err != nil {
		return "", 0, fmt.Errorf("marshaling config: %w", err)
	}

	// Compute diff against previous version if one exists.
	var changelog any
	var prevConfigJSON string
	s.DB.QueryRow("SELECT config FROM _kora_config_version WHERE site = ? AND version = ?", siteName, currentVersion).Scan(&prevConfigJSON)

	if prevConfigJSON != "" {
		var prevDoctypes []*doctype.DocType
		if err := json.Unmarshal([]byte(prevConfigJSON), &prevDoctypes); err == nil {
			diff := doctype.DiffConfigs(prevDoctypes, doctypes)
			diff.FromVersion = currentVersion
			diff.ToVersion = newVersion
			changelogBytes, _ := json.Marshal(diff)
			changelog = string(changelogBytes)
		}
	}

	versionID := fmt.Sprintf("cv-%s-%d", siteName, newVersion)
	_, err = s.DB.Exec(
		`INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, changelog, status, config)
		 VALUES (?, ?, ?, NOW(6), ?, ?, ?, ?, ?)`,
		versionID, siteName, newVersion, createdBy, label, changelog, status, string(configJSON),
	)
	if err != nil {
		return "", 0, fmt.Errorf("creating config version: %w", err)
	}

	slog.Debug("created config version", "id", versionID, "version", newVersion, "status", status)
	return versionID, newVersion, nil
}

// LoadWorkflows loads all workflows from the database.
func (s *Store) LoadWorkflows() ([]*doctype.Workflow, error) {
	// First ensure the table exists.
	s.DB.Exec(`CREATE TABLE IF NOT EXISTS _kora_workflow (
		name VARCHAR(140) PRIMARY KEY, document_type VARCHAR(140) NOT NULL,
		is_active TINYINT(1) NOT NULL DEFAULT 1,
		workflow_state_field VARCHAR(140) NOT NULL DEFAULT 'status', config_json JSON
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)

	rows, err := s.DB.Query("SELECT config_json FROM _kora_workflow WHERE is_active = 1")
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

