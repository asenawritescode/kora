// Package configstore manages reading and writing DocType configuration
// to/from the database (_kora_doctype and _kora_field tables).
package configstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
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
	if err := BumpConfigRevisionTx(dbTx, s.Dialect); err != nil {
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
	_, err = ex.Exec(db.Rebind(s.Dialect, doctypeSQL),
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
		_, err = ex.Exec(db.Rebind(s.Dialect, "DELETE FROM _kora_field WHERE parent = ? AND site = ?"), dt.Name, site)
		if err != nil {
			return fmt.Errorf("deleting fields for %s: %w", dt.Name, err)
		}
		return nil
	}

	// Delete all existing fields for this doctype in one shot,
	// then re-insert all current fields with a batched multi-row INSERT.
	// This replaces the old per-field diff/update/insert loop.
	_, err = ex.Exec(db.Rebind(s.Dialect, "DELETE FROM _kora_field WHERE parent = ? AND site = ?"), dt.Name, site)
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
		args := make([]any, 0, len(batch)*24)
		for j, field := range batch {
			constraintsJSON, _ := json.Marshal(field.Constraints)
			placeholders = append(placeholders, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args,
				fmt.Sprintf("%s.%s", dt.Name, field.Fieldname),
				dt.Name, field.Fieldname, field.Fieldtype, field.Label, field.Options,
				boolToInt(field.Reqd), boolToInt(field.Unique), field.Default,
				boolToInt(field.Hidden), boolToInt(field.ReadOnly), boolToInt(field.Bold),
				boolToInt(field.InListView), boolToInt(field.InStandardFilter),
				boolToInt(field.SearchIndex), field.Description,
				field.DependsOn, field.MandatoryDependsOn, string(constraintsJSON),
				field.RenamedFrom, field.LinkedField, field.Computed, field.Accept, start+j,
				site,
			)
		}

		insertSQL := fmt.Sprintf(
			`INSERT INTO _kora_field (name, parent, fieldname, fieldtype, label, options,
				reqd, unique_constraint, default_value, hidden, read_only, bold,
				in_list_view, in_standard_filter, search_index, description,
					depends_on, mandatory_depends_on, constraints_json, renamed_from, linked_field, computed, accept, idx, site)
			VALUES %s`,
			strings.Join(placeholders, ", "),
		)
		if _, err := ex.Exec(db.Rebind(s.Dialect, insertSQL), args...); err != nil {
			return fmt.Errorf("inserting fields for %s: %w", dt.Name, err)
		}
	}

	slog.Debug("saved doctype config", "name", dt.Name, "fields", len(dt.Fields))
	return nil
}

// LoadAll loads all DocTypes from the database into the registry.
// Uses two batched queries instead of N+1 (one for doctypes, one for all fields).
func (s *Store) LoadAll(site string) ([]*doctype.DocType, error) {
	rows, err := s.DB.Query(db.Rebind(s.Dialect, `
		SELECT name, module, is_submittable, is_child_table, is_single,
			track_changes, title_field, search_fields, sort_field, sort_order, description, config_json
		FROM _kora_doctype
		WHERE site = ? OR site = ''
		ORDER BY name
	`), site)
	if err != nil {
		return nil, fmt.Errorf("querying doctypes: %w", err)
	}
	defer rows.Close()

	var doctypes []*doctype.DocType
	for rows.Next() {
		dt := &doctype.DocType{}
		var isSubmittable, isChildTable, isSingle, trackChanges int
		var configJSON sql.NullString
		err := rows.Scan(
			&dt.Name, &dt.Module, &isSubmittable, &isChildTable, &isSingle,
			&trackChanges, &dt.TitleField, &dt.SearchFields, &dt.SortField,
			&dt.SortOrder, &dt.Description, &configJSON,
		)
		if err != nil {
			return nil, fmt.Errorf("scanning doctype: %w", err)
		}
		if configJSON.Valid && strings.TrimSpace(configJSON.String) != "" {
			var configured doctype.DocType
			if err := json.Unmarshal([]byte(configJSON.String), &configured); err != nil {
				return nil, fmt.Errorf("unmarshaling doctype config %s: %w", dt.Name, err)
			}
			dt = &configured
		}
		// Preserve indexed columns as the source of truth for common listing metadata.
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

// LoadDraftHeadSnapshot returns the latest draft snapshot for a site. If no
// drafts exist, it falls back to the latest active snapshot. If the site has
// no version history, sql.ErrNoRows is returned.
func (s *Store) LoadDraftHeadSnapshot(site string) (*doctype.ConfigSnapshot, string, error) {
	var versionID, config string
	err := s.DB.QueryRow(db.Rebind(s.Dialect, `
		SELECT id, config
		FROM _kora_config_version
		WHERE site = ? AND status IN ('Draft', 'Active')
		ORDER BY CASE WHEN status = 'Draft' THEN 0 ELSE 1 END, version DESC
		LIMIT 1
	`), site).Scan(&versionID, &config)
	if err != nil {
		return nil, "", err
	}

	snapshot, err := doctype.ParseConfig(config)
	if err != nil {
		return nil, "", fmt.Errorf("parsing draft head snapshot %s: %w", versionID, err)
	}
	return snapshot, versionID, nil
}

// LoadActiveSnapshot returns the currently activated tenant configuration.
// Draft snapshots are intentionally excluded because they are not executable.
func (s *Store) LoadActiveSnapshot(site string) (*doctype.ConfigSnapshot, string, error) {
	return s.LoadActiveSnapshotContext(context.Background(), site)
}

// LoadActiveSnapshotContext is LoadActiveSnapshot with caller cancellation.
func (s *Store) LoadActiveSnapshotContext(ctx context.Context, site string) (*doctype.ConfigSnapshot, string, error) {
	var versionID, config string
	err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `
		SELECT id, config FROM _kora_config_version
		WHERE site = ? AND status = 'Active'
		ORDER BY version DESC LIMIT 1
	`), site).Scan(&versionID, &config)
	if err != nil {
		return nil, "", err
	}
	snapshot, err := doctype.ParseConfig(config)
	if err != nil {
		return nil, "", fmt.Errorf("parsing active snapshot %s: %w", versionID, err)
	}
	return snapshot, versionID, nil
}

// BuildDraftSnapshot loads the current draft head (or falls back to the live
// registry state when no version history exists), then inserts or replaces the
// provided doctype in that snapshot.
func (s *Store) BuildDraftSnapshot(reg *doctype.Registry, siteName string, dt *doctype.DocType) (*doctype.ConfigSnapshot, string, error) {
	snapshot, baseVersionID, err := s.LoadDraftHeadSnapshot(siteName)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, "", err
		}
		snapshot, err = s.CollectSnapshot(reg, siteName)
		if err != nil {
			return nil, "", err
		}
		baseVersionID = ""
	}

	doctype.UpsertSnapshotDocType(snapshot, dt)
	return snapshot, baseVersionID, nil
}

// loadAllFields fetches all fields for all doctypes in a single query.
func (s *Store) loadAllFields(site string) (map[string][]doctype.Field, error) {
	rows, err := s.DB.Query(db.Rebind(s.Dialect, `
		SELECT parent, fieldname, fieldtype, label, options, reqd, unique_constraint,
			default_value, hidden, read_only, bold, in_list_view, in_standard_filter,
			search_index, description, depends_on, mandatory_depends_on,
			constraints_json, renamed_from, COALESCE(linked_field,'') as linked_field, COALESCE(computed,'') as computed, COALESCE(accept,'') as accept, idx
		FROM _kora_field
		WHERE site = ? OR site = ''
		ORDER BY parent, idx
	`), site)
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
			&f.RenamedFrom, &f.LinkedField, &f.Computed, &f.Accept, &idxVal,
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
		if strings.TrimSpace(constraintsJSON) != "" && strings.TrimSpace(constraintsJSON) != "null" {
			if err := json.Unmarshal([]byte(constraintsJSON), &f.Constraints); err != nil {
				return nil, fmt.Errorf("unmarshaling constraints for %s.%s: %w", parent, f.Fieldname, err)
			}
		}

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

func configVersionActiveFlag(dialect db.QueryDialect, active bool) any {
	if driver, ok := dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "postgres" {
		return boolToInt(active)
	}
	return active
}

// SaveRoles saves role definitions to _kora_role.
func (s *Store) SaveRoles(roles []*doctype.Role, site string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin role save: %w", err)
	}
	defer tx.Rollback()
	if err := s.SaveRolesTx(tx, roles, site); err != nil {
		return err
	}
	if err := BumpAuthorizationRevisionTx(tx, s.Dialect, true, false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit role save: %w", err)
	}
	return nil
}

// SaveRolesTx is like SaveRoles but uses an existing transaction.
func (s *Store) SaveRolesTx(tx *sql.Tx, roles []*doctype.Role, site string) error {
	for _, role := range roles {
		upsertSQL := `INSERT INTO _kora_role (name, workspace_access, description, site)
			VALUES (?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"workspace_access", "description", "site"})
		_, err := tx.Exec(db.Rebind(s.Dialect, upsertSQL), role.Name, boolToInt(role.WorkspaceAccess), role.Description, site)
		if err != nil {
			return fmt.Errorf("saving role %s: %w", role.Name, err)
		}
	}
	return nil
}

// SavePermissions saves permission definitions to _kora_permission.
func (s *Store) SavePermissions(permissions []*doctype.Permission, site string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin permission save: %w", err)
	}
	defer tx.Rollback()
	if err := s.SavePermissionsTx(tx, permissions, site); err != nil {
		return err
	}
	if err := BumpAuthorizationRevisionTx(tx, s.Dialect, true, false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit permission save: %w", err)
	}
	return nil
}

// SavePermissionsTx is like SavePermissions but uses an existing transaction.
func (s *Store) SavePermissionsTx(tx *sql.Tx, permissions []*doctype.Permission, site string) error {
	for _, p := range permissions {
		name := fmt.Sprintf("%s.%s", p.Doctype, p.Role)
		assignedFields, err := encodeAssignedFields(p.IfAssigned)
		if err != nil {
			return fmt.Errorf("encoding assignment scope for %s: %w", name, err)
		}
		upsertSQL := `INSERT INTO _kora_permission (name, doctype, role, can_read, can_write, can_create,
				can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner, if_assigned, site)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
			[]string{"name"}, []string{"can_read", "can_write", "can_create", "can_delete",
				"can_submit", "can_cancel", "can_amend", "can_export", "can_import", "can_report", "if_owner", "if_assigned", "site"})
		_, err = tx.Exec(db.Rebind(s.Dialect, upsertSQL), name, p.Doctype, p.Role,
			boolToInt(p.Read), boolToInt(p.Write), boolToInt(p.Create),
			boolToInt(p.Delete), boolToInt(p.Submit), boolToInt(p.Cancel),
			boolToInt(p.Amend), boolToInt(p.Export), boolToInt(p.Import),
			boolToInt(p.Report), boolToInt(p.IfOwner), assignedFields, site,
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
	rows, err := s.DB.Query(db.Rebind(s.Dialect, "SELECT name FROM _kora_role WHERE site = ? OR site = ''"), site)
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
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing role query: %w", err)
	}

	// Only Administrator gets auto-granted permissions. Other roles must be
	// explicitly granted access via the Permissions panel (explicit > implicit).
	adminRolePresent := false
	for _, role := range roles {
		if role != "Administrator" {
			continue
		}
		adminRolePresent = true
	}
	if adminRolePresent {
		tx, err := s.DB.Begin()
		if err != nil {
			return fmt.Errorf("begin default permission transaction: %w", err)
		}
		defer tx.Rollback()
		for _, role := range roles {
			if role != "Administrator" {
				continue
			}
			name := fmt.Sprintf("%s.%s", doctypeName, role)
			upsertSQL := `INSERT INTO _kora_permission (name, doctype, role, can_read, can_write, can_create,
				can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner, if_assigned, site)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ` + s.Dialect.UpsertClause(
				[]string{"name"}, []string{"can_read", "can_write", "can_create", "can_delete",
					"can_submit", "can_cancel", "can_amend", "can_export", "can_import", "can_report", "if_owner", "if_assigned", "site"})
			_, err := tx.Exec(db.Rebind(s.Dialect, upsertSQL),
				name, doctypeName, role,
				1, 1, 1, // read, write, create
				1, 1, 1, // delete, submit, cancel
				1, 1, 1, 1, // amend, export, import, report
				0,    // if_owner
				"[]", // if_assigned
				site,
			)
			if err != nil {
				return fmt.Errorf("creating default permission for %s: %w", name, err)
			}
		}
		if err := BumpAuthorizationRevisionTx(tx, s.Dialect, true, false); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit default permission transaction: %w", err)
		}
	}

	// Reload permissions into the in-memory registry so they take effect immediately.
	// The caller's registry will pick these up on the next request.
	slog.Info("auto-created default permissions for new doctype", "doctype", doctypeName, "roles", len(roles))
	return nil
}

// LoadRoles loads all roles from _kora_role.
func (s *Store) LoadRoles(site string) ([]*doctype.Role, error) {
	rows, err := s.DB.Query(db.Rebind(s.Dialect, "SELECT name, workspace_access, description FROM _kora_role WHERE site = ? OR site = '' ORDER BY name"), site)
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
	rows, err := s.DB.Query(db.Rebind(s.Dialect, `
		SELECT doctype, role, can_read, can_write, can_create, can_delete,
			can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner, if_assigned
		FROM _kora_permission
		WHERE site = ? OR site = ''
		ORDER BY doctype, role
	`), site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var perms []*doctype.Permission
	for rows.Next() {
		p := &doctype.Permission{}
		var read, write, create, del, submit, cancel, amend, export, imp, report, ifOwner int
		var assignedFields sql.NullString
		if err := rows.Scan(&p.Doctype, &p.Role, &read, &write, &create, &del,
			&submit, &cancel, &amend, &export, &imp, &report, &ifOwner, &assignedFields); err != nil {
			return nil, err
		}
		if assignedFields.Valid && assignedFields.String != "" {
			if err := json.Unmarshal([]byte(assignedFields.String), &p.IfAssigned); err != nil {
				return nil, fmt.Errorf("decoding assignment scope for %s/%s: %w", p.Role, p.Doctype, err)
			}
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

func encodeAssignedFields(fields []string) (string, error) {
	if len(fields) == 0 {
		return "[]", nil
	}
	encoded, err := json.Marshal(fields)
	return string(encoded), err
}

// CreateConfigVersion saves a snapshot of the full config for version tracking.
// The snapshot includes schema and workspace configuration.
// Returns the version ID, version number, and any error.
func (s *Store) CreateConfigVersion(siteName, createdBy, label, status string, snapshot *doctype.ConfigSnapshot) (string, int, error) {
	var baseVersionID string
	if status == "Draft" {
		err := s.DB.QueryRow(db.Rebind(s.Dialect, "SELECT id FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"), siteName).Scan(&baseVersionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", 0, fmt.Errorf("reading active config version: %w", err)
		}
	}
	return s.CreateConfigVersionWithBase(siteName, createdBy, label, status, snapshot, baseVersionID)
}

// CreateConfigVersionWithBase saves a snapshot of the full config with an
// explicit base version lineage.
func (s *Store) CreateConfigVersionWithBase(siteName, createdBy, label, status string, snapshot *doctype.ConfigSnapshot, baseVersionID string) (string, int, error) {
	if snapshot == nil {
		return "", 0, fmt.Errorf("snapshot is required")
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		versionID, versionNum, err := s.createConfigVersionOnce(siteName, createdBy, label, status, snapshot, baseVersionID)
		if err == nil {
			slog.Debug("created config version", "id", versionID, "version", versionNum, "status", status)
			return versionID, versionNum, nil
		}
		lastErr = err
		if !isRetryableConfigVersionError(err) {
			break
		}
		time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
	}
	return "", 0, fmt.Errorf("creating config version: %w", lastErr)
}

// UpdateDraftConfigVersion replaces the snapshot of an existing inactive
// draft without creating another version. This is used when a conversational
// candidate is repaired after a draft has already been prepared.
func (s *Store) UpdateDraftConfigVersion(siteName, versionID, createdBy, label string, snapshot *doctype.ConfigSnapshot, baseVersionID string) (int, error) {
	if snapshot == nil {
		return 0, fmt.Errorf("snapshot is required")
	}
	if findings := doctype.ValidateSnapshotReferences(snapshot, nil); len(findings) > 0 {
		return 0, &doctype.ReferenceValidationError{Findings: findings}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var version int
	var status string
	if err := tx.QueryRow(db.Rebind(s.Dialect, "SELECT version, status FROM _kora_config_version WHERE site = ? AND id = ?"), siteName, versionID).Scan(&version, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("draft config version %q not found", versionID)
		}
		return 0, fmt.Errorf("reading draft config version: %w", err)
	}
	if status != "Draft" {
		return 0, fmt.Errorf("config version %q is %s, not Draft", versionID, status)
	}
	baseSnapshot, baseVersion, err := loadConfigVersionBase(tx, s.Dialect, siteName, baseVersionID, version)
	if err != nil {
		return 0, err
	}
	changeList, changelog, err := configVersionDiff(baseSnapshot, snapshot, baseVersion, version)
	if err != nil {
		return 0, err
	}

	configSExpr := doctype.ToSExpr(snapshot)
	h := sha256.Sum256([]byte(configSExpr))
	if _, err := tx.Exec(
		db.Rebind(s.Dialect, `UPDATE _kora_config_version
		 SET created_by = ?, label = ?, config = ?, changelog = ?, change_list = ?,
		     config_hash = ?, base_version_id = ?, min_kora_version = ?
		 WHERE site = ? AND id = ? AND status = 'Draft'`),
		createdBy, label, configSExpr, changelog, changeList, hex.EncodeToString(h[:]), baseVersionID,
		snapshot.MinKoraVersion, siteName, versionID,
	); err != nil {
		return 0, fmt.Errorf("updating config version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing config version update: %w", err)
	}
	return version, nil
}

func (s *Store) createConfigVersionOnce(siteName, createdBy, label, status string, snapshot *doctype.ConfigSnapshot, baseVersionID string) (string, int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	versionID, version, err := s.CreateConfigVersionTx(tx, siteName, createdBy, label, status, snapshot, baseVersionID)
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("committing config version: %w", err)
	}
	return versionID, version, nil
}

// CreateConfigVersionTx writes a version using a caller-owned transaction.
// It is used by activation and rollback so the live config and its version
// history become visible together.
func (s *Store) CreateConfigVersionTx(tx *sql.Tx, siteName, createdBy, label, status string, snapshot *doctype.ConfigSnapshot, baseVersionID string) (string, int, error) {
	if tx == nil {
		return "", 0, fmt.Errorf("transaction is required")
	}
	if snapshot == nil {
		return "", 0, fmt.Errorf("snapshot is required")
	}
	if findings := doctype.ValidateSnapshotReferences(snapshot, nil); len(findings) > 0 {
		return "", 0, &doctype.ReferenceValidationError{Findings: findings}
	}
	var currentVersion int
	if err := tx.QueryRow(db.Rebind(s.Dialect, "SELECT COALESCE(MAX(version), 0) FROM _kora_config_version WHERE site = ?"), siteName).Scan(&currentVersion); err != nil {
		return "", 0, fmt.Errorf("reading current version: %w", err)
	}
	newVersion := currentVersion + 1

	if status == "Active" {
		if _, err := tx.Exec(db.Rebind(s.Dialect, "UPDATE _kora_config_version SET status = 'Superseded', is_active = ? WHERE site = ? AND status = 'Active'"), configVersionActiveFlag(s.Dialect, false), siteName); err != nil {
			return "", 0, fmt.Errorf("superseding active versions: %w", err)
		}
	}

	configSExpr := doctype.ToSExpr(snapshot)
	h := sha256.Sum256([]byte(configSExpr))
	configHash := hex.EncodeToString(h[:])

	var changelog any
	var changeList any
	prevSnapshot, baseVersion, err := loadConfigVersionBase(tx, s.Dialect, siteName, baseVersionID, currentVersion)
	if err != nil {
		return "", 0, err
	}
	if baseVersion > 0 {
		changeListValue, changelogValue, diffErr := configVersionDiff(prevSnapshot, snapshot, baseVersion, newVersion)
		if diffErr != nil {
			return "", 0, diffErr
		}
		changeList = changeListValue
		changelog = changelogValue
	}

	versionID := configVersionID(siteName, newVersion)
	minKoraVersion := snapshot.MinKoraVersion
	_, err = tx.Exec(
		db.Rebind(s.Dialect, `INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, changelog, status, is_active, config, change_list, config_hash, base_version_id, min_kora_version)
		 VALUES (?, ?, ?, `+s.Dialect.NowTimestamp()+`, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		versionID, siteName, newVersion, createdBy, label, changelog, status, configVersionActiveFlag(s.Dialect, status == "Active"), configSExpr,
		changeList, configHash, baseVersionID, minKoraVersion,
	)
	if err != nil {
		if isDuplicateConfigVersionError(err) {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("inserting config version: %w", err)
	}
	return versionID, newVersion, nil
}

func loadConfigVersionBase(tx *sql.Tx, dialect db.QueryDialect, siteName, baseVersionID string, beforeVersion int) (*doctype.ConfigSnapshot, int, error) {
	var config string
	var version int
	var err error
	if baseVersionID != "" {
		err = tx.QueryRow(db.Rebind(dialect, "SELECT config, version FROM _kora_config_version WHERE site = ? AND id = ?"), siteName, baseVersionID).Scan(&config, &version)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, fmt.Errorf("base config version %q not found", baseVersionID)
		}
		if err != nil {
			return nil, 0, fmt.Errorf("reading base config version %q: %w", baseVersionID, err)
		}
	}
	if config == "" {
		err = tx.QueryRow(db.Rebind(dialect, "SELECT config, version FROM _kora_config_version WHERE site = ? AND status = 'Active' AND version <= ? ORDER BY version DESC LIMIT 1"), siteName, beforeVersion).Scan(&config, &version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, 0, fmt.Errorf("reading active base config version: %w", err)
		}
	}
	if config == "" {
		return &doctype.ConfigSnapshot{}, 0, nil
	}
	snapshot, err := doctype.ParseConfig(config)
	if err != nil {
		return nil, 0, fmt.Errorf("parsing base config version %d: %w", version, err)
	}
	return snapshot, version, nil
}

func configVersionDiff(previous, next *doctype.ConfigSnapshot, fromVersion, toVersion int) (string, string, error) {
	if previous == nil || next == nil {
		return "", "", fmt.Errorf("config version diff requires both snapshots")
	}
	fullDiff := doctype.DiffFullSnapshots(previous, next)
	fullDiff.Doctypes.FromVersion = fromVersion
	fullDiff.Doctypes.ToVersion = toVersion
	changeListBytes, err := json.Marshal(fullDiff)
	if err != nil {
		return "", "", fmt.Errorf("encoding full config change list: %w", err)
	}
	doctypeDiffBytes, err := json.Marshal(fullDiff.Doctypes)
	if err != nil {
		return "", "", fmt.Errorf("encoding doctype changelog: %w", err)
	}
	return string(changeListBytes), string(doctypeDiffBytes), nil
}

// configVersionID keeps the historical readable ID for short site names while
// staying within the 36-character database column for real tenant hostnames.
func configVersionID(siteName string, version int) string {
	readable := fmt.Sprintf("cv-%s-%d", siteName, version)
	if len(readable) <= 36 {
		return readable
	}
	hash := sha256.Sum256([]byte(siteName))
	return fmt.Sprintf("cv-%s-%d", hex.EncodeToString(hash[:])[:16], version)
}

func isRetryableConfigVersionError(err error) bool {
	if err == nil {
		return false
	}
	return isDuplicateConfigVersionError(err)
}

func isDuplicateConfigVersionError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "duplicate entry") ||
		strings.Contains(s, "unique constraint failed") ||
		strings.Contains(s, "constraint failed") ||
		strings.Contains(s, "duplicate key") ||
		strings.Contains(s, "23505")
}

// SupersedeSiblingDrafts marks draft siblings from the same base snapshot as
// superseded after one draft from that branch has been activated.
func (s *Store) SupersedeSiblingDrafts(siteName, activatedVersionID, baseVersionID string) error {
	if baseVersionID == "" {
		return nil
	}
	_, err := s.DB.Exec(db.Rebind(s.Dialect,
		`UPDATE _kora_config_version
		 SET status = 'Superseded'
		 WHERE site = ? AND status = 'Draft' AND id <> ? AND base_version_id = ?`),
		siteName, activatedVersionID, baseVersionID,
	)
	return err
}

// SupersedeSiblingDraftsTx updates sibling drafts inside the caller's
// activation transaction.
func (s *Store) SupersedeSiblingDraftsTx(tx *sql.Tx, siteName, activatedVersionID, baseVersionID string) error {
	if baseVersionID == "" {
		return nil
	}
	_, err := tx.Exec(db.Rebind(s.Dialect,
		`UPDATE _kora_config_version
		 SET status = 'Superseded'
		 WHERE site = ? AND status = 'Draft' AND id <> ? AND base_version_id = ?`),
		siteName, activatedVersionID, baseVersionID,
	)
	return err
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
	roles, err := s.LoadRoles(site)
	if err != nil {
		return nil, fmt.Errorf("loading roles for snapshot: %w", err)
	}
	permissions, err := s.LoadPermissions(site)
	if err != nil {
		return nil, fmt.Errorf("loading permissions for snapshot: %w", err)
	}
	analyticsMetrics, err := s.LoadAnalyticsMetrics(site)
	if err != nil {
		return nil, fmt.Errorf("loading analytics metrics for snapshot: %w", err)
	}
	scripts, err := s.LoadScriptSnapshots(site)
	if err != nil {
		return nil, fmt.Errorf("loading scripts for snapshot: %w", err)
	}
	views, err := s.LoadViews(site)
	if err != nil {
		return nil, fmt.Errorf("loading views for snapshot: %w", err)
	}
	// Reports and MinKoraVersion currently exist only in versioned snapshot
	// history. Carry those fields forward from the active head so an additive
	// package activation cannot silently erase them while collecting live rows.
	var activeConfig string
	err = s.DB.QueryRow(db.Rebind(s.Dialect, "SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"), site).Scan(&activeConfig)
	var reports []json.RawMessage
	var minKoraVersion string
	var scheduledJobs []contract.ScheduledJobDefinition
	var modules []contract.ModuleDescriptor
	var publishedBindings []contract.ConfigurationSurfaceBinding
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("loading active config snapshot metadata: %w", err)
	}
	if err == nil {
		activeSnapshot, parseErr := doctype.ParseConfig(activeConfig)
		if parseErr != nil {
			return nil, fmt.Errorf("parsing active config snapshot metadata: %w", parseErr)
		}
		reports = append([]json.RawMessage(nil), activeSnapshot.Reports...)
		minKoraVersion = activeSnapshot.MinKoraVersion
		scheduledJobs = append([]contract.ScheduledJobDefinition(nil), activeSnapshot.ScheduledJobs...)
		modules = append([]contract.ModuleDescriptor(nil), activeSnapshot.Modules...)
		publishedBindings = append([]contract.ConfigurationSurfaceBinding(nil), activeSnapshot.PublishedBindings...)
	}
	return &doctype.ConfigSnapshot{
		DocTypes:          doctypes,
		Roles:             roles,
		Permissions:       permissions,
		Views:             views,
		AnalyticsMetrics:  analyticsMetrics,
		Scripts:           scripts,
		Reports:           reports,
		MinKoraVersion:    minKoraVersion,
		ScheduledJobs:     scheduledJobs,
		Modules:           modules,
		PublishedBindings: publishedBindings,
	}, nil
}

// LoadAnalyticsMetrics loads all custom analytics metric definitions.
func (s *Store) LoadAnalyticsMetrics(site string) ([]*doctype.AnalyticsMetricConfig, error) {
	rows, err := s.DB.Query(db.Rebind(s.Dialect, "SELECT name, label, type, doctype, field_name, link_field, group_by_field FROM _kora_analytics_metric WHERE site = ? OR site = ''"), site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metrics []*doctype.AnalyticsMetricConfig
	for rows.Next() {
		var m doctype.AnalyticsMetricConfig
		if err := rows.Scan(&m.Name, &m.Label, &m.Type, &m.DocType, &m.FieldName, &m.LinkField, &m.GroupByField); err != nil {
			return nil, fmt.Errorf("scanning analytics metric: %w", err)
		}
		m.AutoGenerated = false
		metrics = append(metrics, &m)
	}
	return metrics, rows.Err()
}

// SaveAnalyticsMetrics saves custom analytics metric definitions from a snapshot.
func (s *Store) SaveAnalyticsMetrics(metrics []*doctype.AnalyticsMetricConfig, site string) error {
	if len(metrics) == 0 {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin analytics metric save: %w", err)
	}
	defer tx.Rollback()
	if err := s.SaveAnalyticsMetricsTx(tx, metrics, site); err != nil {
		return err
	}
	if err := BumpConfigRevisionTx(tx, s.Dialect); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit analytics metric save: %w", err)
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
			` + s.Dialect.UpsertClause([]string{"site", "name"}, []string{"label", "type", "doctype", "field_name", "link_field", "group_by_field"})
		_, err := tx.Exec(db.Rebind(s.Dialect, upsertSQL), m.Name, m.Label, m.Type, m.DocType, m.FieldName, m.LinkField, m.GroupByField, site)
		if err != nil {
			return fmt.Errorf("saving analytics metric %s: %w", m.Name, err)
		}
	}
	return nil
}

// LoadScriptSnapshots loads all active scripts as snapshots for versioning.
// Script bodies are hashed (SHA-256) rather than stored inline to avoid DB bloat.
func (s *Store) LoadScriptSnapshots(site string) ([]*doctype.ScriptSnapshot, error) {
	rows, err := s.DB.Query(db.Rebind(s.Dialect, `SELECT name, script_type, doctype, event, method_path, schedule,
		priority, is_active, run_as, timeout_ms, script FROM _kora_script WHERE is_active = ? AND (site = ? OR site = '')`), true, site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var scripts []*doctype.ScriptSnapshot
	for rows.Next() {
		var ss doctype.ScriptSnapshot
		var script string
		if err := rows.Scan(&ss.Name, &ss.ScriptType, &ss.DocType, &ss.Event, &ss.MethodPath,
			&ss.Schedule, &ss.Priority, &ss.IsActive, &ss.RunAs,
			&ss.TimeoutMs, &script); err != nil {
			return nil, err
		}
		ss.Source = script
		h := sha256.Sum256([]byte(script))
		ss.ScriptHash = hex.EncodeToString(h[:])
		scripts = append(scripts, &ss)
	}
	return scripts, rows.Err()
}

// SaveScriptsTx persists script definitions as part of config activation. The
// source body is taken from the immutable version snapshot, so the reviewed
// body and its metadata become live atomically with the rest of the snapshot.
func (s *Store) SaveScriptsTx(tx *sql.Tx, scripts []*doctype.ScriptSnapshot, site string, dialect db.Dialect) error {
	for _, ss := range scripts {
		if ss == nil {
			continue
		}
		upsertSQL := `INSERT INTO _kora_script (name, site, script_type, doctype, event, method_path, schedule,
			priority, is_active, run_as, timeout_ms, script)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			` + dialect.UpsertClause([]string{"name"}, []string{"script_type", "doctype", "event", "method_path", "schedule",
			"priority", "is_active", "run_as", "timeout_ms", "script"})
		if _, err := tx.Exec(db.Rebind(dialect, upsertSQL), ss.Name, site, ss.ScriptType, ss.DocType, ss.Event, ss.MethodPath,
			ss.Schedule, ss.Priority, ss.IsActive, ss.RunAs, ss.TimeoutMs, ss.Source); err != nil {
			return fmt.Errorf("saving script %s during activation: %w", ss.Name, err)
		}
	}
	return nil
}

// SaveScripts upserts active scripts from a config snapshot during site provisioning.
// Existing scripts with the same name are updated; new ones are inserted.
func (s *Store) SaveScripts(scripts []*doctype.ScriptSnapshot, scriptBodyByHash map[string]string, site string) error {
	if len(scripts) == 0 {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("begin script snapshot save: %w", err)
	}
	defer tx.Rollback()
	for _, ss := range scripts {
		if ss == nil {
			continue
		}
		scriptBody := scriptBodyByHash[ss.ScriptHash]
		upsertSQL := `INSERT INTO _kora_script (name, site, script_type, doctype, event, method_path, schedule,
			priority, is_active, run_as, timeout_ms, script)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			` + s.Dialect.UpsertClause([]string{"name"}, []string{"script_type", "doctype", "event", "method_path", "schedule",
			"priority", "is_active", "run_as", "timeout_ms", "script"})
		_, err := tx.Exec(db.Rebind(s.Dialect, upsertSQL), ss.Name, site, ss.ScriptType, ss.DocType, ss.Event, ss.MethodPath,
			ss.Schedule, ss.Priority, ss.IsActive, ss.RunAs, ss.TimeoutMs, scriptBody)
		if err != nil {
			return fmt.Errorf("saving script %s: %w", ss.Name, err)
		}
	}
	if err := BumpConfigRevisionTx(tx, s.Dialect); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit script snapshot save: %w", err)
	}
	return nil
}

// ActivateSnapshot runs the full activation in a single transaction.
// 1. Writes all config rows (doctypes, roles, permissions, metrics)
// 2. Applies DDL (caller must apply DDL after the tx, or use tx.Exec for SQLite)
// Registry mutation is intentionally left to the caller after commit so a
// rolled-back SQL transaction cannot leave process-local config ahead of SQL.
// On any failure, returns an error (caller should roll back the transaction).
func (s *Store) ActivateSnapshot(tx *sql.Tx, snapshot *doctype.ConfigSnapshot, siteName string, dialect db.Dialect) error {
	return s.activateSnapshot(tx, snapshot, siteName, dialect, func() error {
		return BumpAuthorizationRevisionTx(tx, dialect, true, false)
	})
}

// ActivateSnapshotAtManagementClaim validates the latest tenant-local claim
// fence and the expected config revision in the same transaction as the
// snapshot writes. A newer retry can therefore fence this attempt before it
// commits any configuration effects.
func (s *Store) ActivateSnapshotAtManagementClaim(ctx context.Context, tx *sql.Tx, snapshot *doctype.ConfigSnapshot, siteName string, dialect db.Dialect, operationID string, fence, expectedRevision uint64, digest string) error {
	if err := ValidateManagementActivationFenceTx(ctx, tx, dialect, siteName, operationID, fence, expectedRevision, digest); err != nil {
		return err
	}
	return s.ActivateSnapshotAtConfigRevision(tx, snapshot, siteName, dialect, expectedRevision)
}

// ActivateSnapshotAtManagementClaimWithJournal commits the tenant config,
// revision, authorization epoch and activation journal phase under the same
// current management claim. The caller still owns DDL planning and runtime
// generation reconciliation after this transaction commits.
func (s *Store) ActivateSnapshotAtManagementClaimWithJournal(ctx context.Context, tx *sql.Tx, snapshot *doctype.ConfigSnapshot, siteName string, dialect db.Dialect, operationID, journalKind, fromPhase, recoveryPlan string, fence, expectedRevision uint64, digest string) error {
	if journalKind == "" || fromPhase == "" {
		return fmt.Errorf("management activation journal identity and source phase are required")
	}
	if err := ValidateManagementActivationFenceTx(ctx, tx, dialect, siteName, operationID, fence, expectedRevision, digest); err != nil {
		return err
	}
	if err := s.ActivateSnapshotAtConfigRevision(tx, snapshot, siteName, dialect, expectedRevision); err != nil {
		return err
	}
	return UpdateActivationOperationPhaseTx(tx, dialect, siteName, operationID, journalKind, fromPhase, ActivationActivated, recoveryPlan)
}

// ManagementActivationCommit identifies the already-validated durable
// management activation whose config, active version, and journal phase must
// become visible atomically. DDL planning/application is intentionally a
// preceding journaled phase because MySQL DDL may commit statement by statement.
type ManagementActivationCommit struct {
	OperationID            string
	JournalKind            string
	FromPhase              string
	RecoveryPlan           string
	CreatedBy              string
	VersionLabel           string
	Fence                  uint64
	ExpectedRevision       uint64
	ArtifactDigest         string
	ModuleTenantID         contract.TenantID
	ModuleSnapshotRevision uint64
	ProviderSelections     []contract.ProviderSelection
	PackageConstraints     []contract.InstalledPackage
}

func mergeInstalledPackages(existing, incoming []contract.InstalledPackage) []contract.InstalledPackage {
	byRelease := make(map[string]contract.InstalledPackage, len(existing)+len(incoming))
	for _, pkg := range existing {
		byRelease[pkg.ReleaseID] = pkg
	}
	for _, pkg := range incoming {
		byRelease[pkg.ReleaseID] = pkg
	}
	out := make([]contract.InstalledPackage, 0, len(byRelease))
	for _, pkg := range byRelease {
		out = append(out, pkg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReleaseID < out[j].ReleaseID })
	return out
}

// CommitManagementActivation validates the current worker claim and atomically
// writes tenant config, revision epochs, active config-version history, and the
// activation journal's activated phase. The caller owns transaction commit or
// rollback and must publish/reconcile the process runtime only after commit.
func (s *Store) CommitManagementActivation(ctx context.Context, tx *sql.Tx, snapshot *doctype.ConfigSnapshot, siteName string, dialect db.Dialect, commit ManagementActivationCommit) (string, int, error) {
	if s == nil || s.Dialect == nil || tx == nil || snapshot == nil {
		return "", 0, fmt.Errorf("management activation commit requires store, transaction, and snapshot")
	}
	if siteName == "" || commit.OperationID == "" || commit.JournalKind == "" || commit.FromPhase != ActivationValidated || commit.CreatedBy == "" || commit.VersionLabel == "" {
		return "", 0, fmt.Errorf("management activation commit identity, validated phase, and version metadata are required")
	}
	if err := ValidateManagementActivationFenceTx(ctx, tx, dialect, siteName, commit.OperationID, commit.Fence, commit.ExpectedRevision, commit.ArtifactDigest); err != nil {
		return "", 0, err
	}
	if err := s.ActivateSnapshotAtConfigRevision(tx, snapshot, siteName, dialect, commit.ExpectedRevision); err != nil {
		return "", 0, err
	}
	if len(snapshot.Modules) > 0 || len(commit.PackageConstraints) > 0 {
		if _, err := contract.ParseTenantID(string(commit.ModuleTenantID)); err != nil {
			return "", 0, fmt.Errorf("module snapshot tenant identity is invalid: %w", err)
		}
		moduleStore := NewModuleStore(s.DB, dialect)
		// The executor ensures module tables before opening this transaction.
		// DDL here would implicitly commit prior configuration writes on MySQL.
		tenantID := commit.ModuleTenantID
		existing, err := loadModuleSnapshotTx(ctx, tx, dialect, tenantID)
		if err != nil {
			return "", 0, err
		}
		if existing.Revision != commit.ModuleSnapshotRevision {
			return "", 0, fmt.Errorf("module snapshot revision conflict: expected %d, actual %d", commit.ModuleSnapshotRevision, existing.Revision)
		}
		allModules := mergeInstalledDescriptors(existing.Modules, snapshot.Modules)
		ownership, err := replaceModuleOwnership(existing.Ownership, tenantID, snapshot.Modules)
		if err != nil {
			return "", 0, err
		}
		selections := existing.ProviderSelections
		if commit.ProviderSelections != nil {
			selections = commit.ProviderSelections
		}
		packages := mergeInstalledPackages(existing.Packages, commit.PackageConstraints)
		moduleSnapshot := contract.ModuleSnapshot{TenantID: tenantID, Modules: allModules, Packages: packages, Installations: replaceModuleInstallations(existing.Installations, tenantID, snapshot.Modules, commit.ArtifactDigest), Ownership: ownership, ProviderSelections: append([]contract.ProviderSelection(nil), selections...)}
		if err := validateModuleSnapshot(moduleSnapshot); err != nil {
			return "", 0, err
		}
		if _, err := moduleStore.CommitActivationTx(ctx, tx, commit.ModuleSnapshotRevision, moduleSnapshot); err != nil {
			return "", 0, err
		}
	}
	versionID, version, err := s.CreateConfigVersionTx(tx, siteName, commit.CreatedBy, commit.VersionLabel, "Active", snapshot, "")
	if err != nil {
		return "", 0, fmt.Errorf("recording management activation config version: %w", err)
	}
	if err := UpdateActivationOperationPhaseTx(tx, dialect, siteName, commit.OperationID, commit.JournalKind, commit.FromPhase, ActivationActivated, commit.RecoveryPlan); err != nil {
		return "", 0, err
	}
	return versionID, version, nil
}

// ActivateSnapshotAtConfigRevision is the management package/config activation
// path. Its revision compare-and-set shares the transaction with all config
// rows so stale operations cannot publish a partial or superseded snapshot.
func (s *Store) ActivateSnapshotAtConfigRevision(tx *sql.Tx, snapshot *doctype.ConfigSnapshot, siteName string, dialect db.Dialect, expectedRevision uint64) error {
	return s.activateSnapshot(tx, snapshot, siteName, dialect, func() error {
		return BumpAuthorizationRevisionAtConfigRevisionTx(tx, dialect, expectedRevision)
	})
}

func (s *Store) activateSnapshot(tx *sql.Tx, snapshot *doctype.ConfigSnapshot, siteName string, dialect db.Dialect, advanceRevision func() error) error {
	if tx == nil || snapshot == nil {
		return fmt.Errorf("activation requires transaction and config snapshot")
	}
	if advanceRevision == nil {
		return fmt.Errorf("activation requires a revision update")
	}
	if err := advanceRevision(); err != nil {
		return err
	}
	// Step 1: Save all doctypes using the transaction.
	for _, dt := range snapshot.DocTypes {
		if err := s.SaveDocTypeTx(tx, dt, siteName); err != nil {
			return fmt.Errorf("saving doctype %s during activation: %w", dt.Name, err)
		}
	}

	// Step 2: Save roles, permissions, and analytics metrics.
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
	if len(snapshot.AnalyticsMetrics) > 0 {
		if err := s.SaveAnalyticsMetricsTx(tx, snapshot.AnalyticsMetrics, siteName); err != nil {
			return fmt.Errorf("saving analytics metrics during activation: %w", err)
		}
	}
	if len(snapshot.Views) > 0 {
		if err := s.SaveViewsTx(tx, snapshot.Views, siteName); err != nil {
			return fmt.Errorf("saving views during activation: %w", err)
		}
	}
	if len(snapshot.Scripts) > 0 {
		if err := s.SaveScriptsTx(tx, snapshot.Scripts, siteName, dialect); err != nil {
			return err
		}
	}

	return nil
}

// ApplySnapshotToRegistry installs a committed configuration snapshot into
// the process-local registry. Keep this separate from ActivateSnapshot so a
// transaction rollback cannot leave in-memory routing/config ahead of SQL.
func ApplySnapshotToRegistry(snapshot *doctype.ConfigSnapshot, reg *doctype.Registry) {
	if snapshot == nil || reg == nil {
		return
	}
	reg.LoadFull(snapshot.DocTypes, snapshot.Roles, snapshot.Permissions)
	reg.Views.LoadFromDB(snapshot.Views)
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
		db.Rebind(s.Dialect, "SELECT id, config FROM _kora_config_version WHERE site = ? AND config != '' AND config IS NOT NULL"),
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
		_, err = s.DB.Exec(db.Rebind(s.Dialect, "UPDATE _kora_config_version SET config = ? WHERE id = ?"), sexpr, m.id)
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
	rows, err := s.DB.Query(db.Rebind(s.Dialect, "SELECT DISTINCT site FROM _kora_config_version WHERE config != '' AND config IS NOT NULL"))
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
