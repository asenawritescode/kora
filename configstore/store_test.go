package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	koraDB "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func newStore(database *sql.DB) *Store {
	return &Store{DB: database, Dialect: &koraDB.MySQLDialect{}}
}

func TestLoadAll_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	// Expect doctype query — returns empty.
	mock.ExpectQuery("SELECT name, module, is_submittable, is_child_table, is_single, track_changes, title_field, search_fields, sort_field, sort_order, description, config_json FROM _kora_doctype WHERE site = \\? OR site = '' ORDER BY name").
		WillReturnRows(sqlmock.NewRows([]string{"name", "module", "is_submittable", "is_child_table", "is_single", "track_changes", "title_field", "search_fields", "sort_field", "sort_order", "description", "config_json"}))

	// Expect field query — also empty.
	mock.ExpectQuery("SELECT .* FROM _kora_field WHERE .* ORDER BY parent, idx").
		WillReturnRows(sqlmock.NewRows([]string{
			"parent", "fieldname", "fieldtype", "label", "options",
			"reqd", "unique_constraint", "default_value", "hidden", "read_only",
			"bold", "in_list_view", "in_standard_filter", "search_index",
			"description", "depends_on", "mandatory_depends_on", "constraints_json",
			"renamed_from", "linked_field", "computed", "accept", "idx",
		}))

	doctypes, err := s.LoadAll("")
	if err != nil {
		t.Fatalf("LoadAll error = %v", err)
	}
	if len(doctypes) != 0 {
		t.Errorf("LoadAll returned %d doctypes, want 0", len(doctypes))
	}
}

func TestLoadActiveSnapshotLoadsScheduledDefinitionsOnlyFromActiveVersion(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	store := newStore(database)
	config := doctype.ToSExpr(&doctype.ConfigSnapshot{ScheduledJobs: []contract.ScheduledJobDefinition{{Name: "daily", Type: "custom", Schedule: "0 0 * * *"}}})
	mock.ExpectQuery("SELECT id, config FROM _kora_config_version WHERE site = \\? AND status = 'Active' ORDER BY version DESC LIMIT 1").
		WithArgs("tenant-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "config"}).AddRow("version-1", config))

	snapshot, versionID, err := store.LoadActiveSnapshot("tenant-a")
	if err != nil {
		t.Fatal("LoadActiveSnapshot:", err)
	}
	if versionID != "version-1" || len(snapshot.ScheduledJobs) != 1 || snapshot.ScheduledJobs[0].Name != "daily" {
		t.Fatalf("active snapshot = (%q, %#v), want version and scheduled definition", versionID, snapshot)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectation:", err)
	}
}

func TestLoadAllFieldsRebindsPostgresPlaceholders(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()
	store := &Store{DB: database, Dialect: &koraDB.PostgresDialect{}}
	mock.ExpectQuery("SELECT parent, fieldname, fieldtype, label, options, reqd, unique_constraint, default_value, hidden, read_only, bold, in_list_view, in_standard_filter, search_index, description, depends_on, mandatory_depends_on, constraints_json, renamed_from, COALESCE\\(linked_field,''\\) as linked_field, COALESCE\\(computed,''\\) as computed, COALESCE\\(accept,''\\) as accept, idx FROM _kora_field WHERE site = \\$1 OR site = '' ORDER BY parent, idx").
		WithArgs("tenant").
		WillReturnRows(sqlmock.NewRows([]string{"parent", "fieldname", "fieldtype", "label", "options", "reqd", "unique_constraint", "default_value", "hidden", "read_only", "bold", "in_list_view", "in_standard_filter", "search_index", "description", "depends_on", "mandatory_depends_on", "constraints_json", "renamed_from", "linked_field", "computed", "accept", "idx"}))
	if _, err := store.loadAllFields("tenant"); err != nil {
		t.Fatalf("loadAllFields: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAll_WithDoctypes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	// Doctype rows.
	taskConfig := `{"name":"Task","module":"Core","title_field":"subject","public_access":{"enabled":true,"list":true,"fields":["name","subject"]}}`
	dtRows := sqlmock.NewRows([]string{"name", "module", "is_submittable", "is_child_table", "is_single", "track_changes", "title_field", "search_fields", "sort_field", "sort_order", "description", "config_json"}).
		AddRow("Task", "Core", 1, 0, 0, 0, "subject", "subject,status", "modified", "DESC", "A task", taskConfig).
		AddRow("User", "Core", 0, 0, 0, 0, "name", "name,email", "modified", "DESC", "A user", "")

	mock.ExpectQuery("SELECT name, module, is_submittable, is_child_table, is_single, track_changes, title_field, search_fields, sort_field, sort_order, description, config_json FROM _kora_doctype WHERE site = \\? OR site = '' ORDER BY name").
		WillReturnRows(dtRows)

	// Field rows.
	fieldRows := sqlmock.NewRows([]string{
		"parent", "fieldname", "fieldtype", "label", "options",
		"reqd", "unique_constraint", "default_value", "hidden", "read_only",
		"bold", "in_list_view", "in_standard_filter", "search_index",
		"description", "depends_on", "mandatory_depends_on", "constraints_json",
		"renamed_from", "linked_field", "computed", "accept", "idx",
	}).
		AddRow("Task", "subject", "Data", "Subject", "", 1, 0, "", 0, 0, 0, 1, 0, 0, "", "", "", `[{"type":"min","value":1,"message":"Subject is required."}]`, "", "", "", "", 0).
		AddRow("Task", "status", "Select", "Status", "Open,Closed", 1, 0, "Open", 0, 0, 0, 1, 0, 0, "", "", "", "[]", "", "", "", "", 1).
		AddRow("User", "email", "Data", "Email", "", 1, 1, "", 0, 0, 0, 1, 0, 0, "", "", "", "[]", "", "", "", "", 0)

	mock.ExpectQuery("SELECT .* FROM _kora_field WHERE .* ORDER BY parent, idx").
		WillReturnRows(fieldRows)

	doctypes, err := s.LoadAll("")
	if err != nil {
		t.Fatalf("LoadAll error = %v", err)
	}
	if len(doctypes) != 2 {
		t.Fatalf("LoadAll returned %d doctypes, want 2", len(doctypes))
	}

	// Verify first doctype.
	if doctypes[0].Name != "Task" {
		t.Errorf("doctypes[0].Name = %q, want %q", doctypes[0].Name, "Task")
	}
	if !doctypes[0].IsSubmittable {
		t.Error("Task should be submittable")
	}
	if len(doctypes[0].Fields) != 2 {
		t.Fatalf("Task has %d fields, want 2", len(doctypes[0].Fields))
	}
	if doctypes[0].Fields[0].Fieldname != "subject" {
		t.Errorf("Task field[0] = %q, want %q", doctypes[0].Fields[0].Fieldname, "subject")
	}
	if !doctypes[0].Fields[0].Reqd {
		t.Error("subject should be required")
	}
	if len(doctypes[0].Fields[0].Constraints) != 1 || doctypes[0].Fields[0].Constraints[0].Type != "min" {
		t.Fatalf("subject constraints = %#v, want persisted min constraint", doctypes[0].Fields[0].Constraints)
	}
	if doctypes[0].PublicAccess == nil || !doctypes[0].PublicAccess.Enabled {
		t.Fatal("Task public access should be loaded from config_json")
	}
	if got := doctypes[0].PublicAccess.Fields; len(got) != 2 || got[1] != "subject" {
		t.Fatalf("Task public fields = %#v, want name and subject", got)
	}

	// Verify second doctype.
	if doctypes[1].Name != "User" {
		t.Errorf("doctypes[1].Name = %q, want %q", doctypes[1].Name, "User")
	}
	if len(doctypes[1].Fields) != 1 {
		t.Fatalf("User has %d fields, want 1", len(doctypes[1].Fields))
	}
	if doctypes[1].Fields[0].Unique != true {
		t.Error("email should have unique constraint")
	}
}

func TestSaveRoles_RoundTrip(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	roles := []*doctype.Role{
		{Name: "Admin", WorkspaceAccess: true, Description: "Administrator"},
		{Name: "Editor", WorkspaceAccess: true, Description: "Editor role"},
	}
	mock.ExpectBegin()

	for _, role := range roles {
		expectUpsert := `INSERT INTO _kora_role`
		mock.ExpectExec(expectUpsert).
			WithArgs(role.Name, 1, role.Description, "").
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectExec(`UPDATE _kora_auth_revision SET policy_revision = policy_revision \+ 1 WHERE id = 1`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_config_revision SET revision = revision \+ 1 WHERE id = 1 AND revision < 9223372036854775807`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = s.SaveRoles(roles, "")
	if err != nil {
		t.Fatalf("SaveRoles error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestLoadPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	rows := sqlmock.NewRows([]string{
		"doctype", "role", "can_read", "can_write", "can_create", "can_delete",
		"can_submit", "can_cancel", "can_amend", "can_export", "can_import",
		"can_report", "if_owner", "if_assigned",
	}).
		AddRow("Task", "Admin", 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, "[]").
		AddRow("Task", "Editor", 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, `["assigned_to"]`).
		AddRow("User", "Admin", 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, nil)

	mock.ExpectQuery("SELECT doctype, role, can_read, can_write, can_create, can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner, if_assigned FROM _kora_permission WHERE site = \\? OR site = '' ORDER BY doctype, role").
		WillReturnRows(rows)

	perms, err := s.LoadPermissions("")
	if err != nil {
		t.Fatalf("LoadPermissions error = %v", err)
	}
	if len(perms) != 3 {
		t.Fatalf("LoadPermissions returned %d permissions, want 3", len(perms))
	}

	// Verify Admin/Task has full access.
	if perms[0].Doctype != "Task" || perms[0].Role != "Admin" {
		t.Errorf("perms[0] = %s/%s, want Task/Admin", perms[0].Doctype, perms[0].Role)
	}
	if !perms[0].Read || !perms[0].Write || !perms[0].Delete {
		t.Error("Admin/Task should have read/write/delete")
	}

	// Verify Editor/Task has limited access.
	if perms[1].Doctype != "Task" || perms[1].Role != "Editor" {
		t.Errorf("perms[1] = %s/%s, want Task/Editor", perms[1].Doctype, perms[1].Role)
	}
	if len(perms[1].IfAssigned) != 1 || perms[1].IfAssigned[0] != "assigned_to" {
		t.Fatalf("assignment scope = %#v, want assigned_to", perms[1].IfAssigned)
	}
	if perms[1].Delete {
		t.Error("Editor/Task should NOT have delete")
	}
}

func TestLoadScriptSnapshots(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	rows := sqlmock.NewRows([]string{
		"name", "script_type", "doctype", "event", "method_path",
		"schedule", "priority", "is_active", "run_as",
		"timeout_ms", "script",
	}).
		AddRow("validate_task", "doc_event", "Task", "before_save", "", "", 10, 1, "", 5000, "console.log('validating')").
		AddRow("daily_report", "scheduled", "", "", "", "0 9 * * *", 5, 1, "Admin", 30000, "console.log('report')")

	mock.ExpectQuery("SELECT name, script_type, doctype, event, method_path, schedule, priority, is_active, run_as, timeout_ms, script FROM _kora_script WHERE is_active = \\?").
		WithArgs(true, "").
		WillReturnRows(rows)

	snapshots, err := s.LoadScriptSnapshots("")
	if err != nil {
		t.Fatalf("LoadScriptSnapshots error = %v", err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("LoadScriptSnapshots returned %d, want 2", len(snapshots))
	}

	if snapshots[0].Name != "validate_task" {
		t.Errorf("snapshots[0].Name = %q, want %q", snapshots[0].Name, "validate_task")
	}
	if snapshots[0].ScriptHash == "" {
		t.Error("ScriptHash should not be empty")
	}

	if snapshots[1].Name != "daily_report" {
		t.Errorf("snapshots[1].Name = %q, want %q", snapshots[1].Name, "daily_report")
	}
	if snapshots[1].TimeoutMs != 30000 {
		t.Errorf("snapshots[1].TimeoutMs = %d, want %d", snapshots[1].TimeoutMs, 30000)
	}
}

func TestLoadRoles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	rows := sqlmock.NewRows([]string{"name", "workspace_access", "description"}).
		AddRow("Admin", 1, "Administrator").
		AddRow("Guest", 0, "Guest access")

	mock.ExpectQuery("SELECT name, workspace_access, description FROM _kora_role WHERE site = \\? OR site = '' ORDER BY name").
		WillReturnRows(rows)

	roles, err := s.LoadRoles("")
	if err != nil {
		t.Fatalf("LoadRoles error = %v", err)
	}
	if len(roles) != 2 {
		t.Fatalf("LoadRoles returned %d, want 2", len(roles))
	}
	if !roles[0].WorkspaceAccess {
		t.Error("Admin should have workspace access")
	}
	if roles[1].WorkspaceAccess {
		t.Error("Guest should NOT have workspace access")
	}
}

func TestSavePermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	perms := []*doctype.Permission{
		{
			Doctype: "Task",
			Role:    "Admin",
			Read:    true,
			Write:   true,
			Create:  true,
			Delete:  true,
		},
		{
			Doctype:    "Task",
			Role:       "Guest",
			Read:       true,
			IfAssigned: []string{"assigned_to"},
		},
	}
	mock.ExpectBegin()

	mock.ExpectExec("INSERT INTO _kora_permission").
		WithArgs("Task.Admin", "Task", "Admin", 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, "[]", "").
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec("INSERT INTO _kora_permission").
		WithArgs("Task.Guest", "Task", "Guest", 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, `["assigned_to"]`, "").
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec(`UPDATE _kora_auth_revision SET policy_revision = policy_revision \+ 1 WHERE id = 1`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_config_revision SET revision = revision \+ 1 WHERE id = 1 AND revision < 9223372036854775807`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = s.SavePermissions(perms, "")
	if err != nil {
		t.Fatalf("SavePermissions error = %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBoolToInt(t *testing.T) {
	tests := []struct {
		input bool
		want  int
	}{
		{true, 1},
		{false, 0},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := boolToInt(tt.input)
			if got != tt.want {
				t.Errorf("boolToInt(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadDraftHeadSnapshot_PrefersLatestDraft(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)
	rows := sqlmock.NewRows([]string{"id", "config"}).
		AddRow("cv-test-3", `{"doctypes":[{"name":"Invoice","module":"Accounts","fields":[{"fieldname":"total","fieldtype":"Currency"}]}]}`)

	mock.ExpectQuery("SELECT id, config FROM _kora_config_version").
		WithArgs("test").
		WillReturnRows(rows)

	snapshot, versionID, err := s.LoadDraftHeadSnapshot("test")
	if err != nil {
		t.Fatalf("LoadDraftHeadSnapshot error = %v", err)
	}
	if versionID != "cv-test-3" {
		t.Fatalf("versionID = %q, want cv-test-3", versionID)
	}
	if len(snapshot.DocTypes) != 1 || snapshot.DocTypes[0].Name != "Invoice" {
		t.Fatalf("unexpected snapshot doctypes: %+v", snapshot.DocTypes)
	}
}

func TestCreateConfigVersionWithBase_UsesExplicitBaseVersionID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)
	snapshot := &doctype.ConfigSnapshot{
		DocTypes: []*doctype.DocType{{Name: "Invoice", Module: "Accounts", Fields: []doctype.Field{{Fieldname: "total", Fieldtype: "Currency"}}}},
	}

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(version\\), 0\\) FROM _kora_config_version WHERE site = \\?").
		WithArgs("test").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(3))
	mock.ExpectQuery("SELECT config, version FROM _kora_config_version WHERE site = \\? AND id = \\?").
		WithArgs("test", "cv-test-2").
		WillReturnRows(sqlmock.NewRows([]string{"config", "version"}).AddRow(doctype.ToSExpr(&doctype.ConfigSnapshot{}), 2))
	mock.ExpectExec("INSERT INTO _kora_config_version").
		WithArgs(
			"cv-test-4", "test", 4, "system", "Draft invoice", sqlmock.AnyArg(), "Draft", false, sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "cv-test-2", "",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	versionID, versionNum, err := s.CreateConfigVersionWithBase("test", "system", "Draft invoice", "Draft", snapshot, "cv-test-2")
	if err != nil {
		t.Fatalf("CreateConfigVersionWithBase error = %v", err)
	}
	if versionID != "cv-test-4" || versionNum != 4 {
		t.Fatalf("unexpected version result: id=%s version=%d", versionID, versionNum)
	}
}

func TestCreateConfigVersionActiveSupersedesPreviousActiveAndSetsFlag(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)
	snapshot := &doctype.ConfigSnapshot{
		DocTypes: []*doctype.DocType{{Name: "Invoice", Module: "Accounts"}},
	}

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(version\\), 0\\) FROM _kora_config_version WHERE site = \\?").
		WithArgs("test").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(3))
	mock.ExpectExec("UPDATE _kora_config_version SET status = 'Superseded', is_active = \\? WHERE site = \\? AND status = 'Active'").
		WithArgs(false, "test").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT config, version FROM _kora_config_version WHERE site = \\? AND id = \\?").
		WithArgs("test", "cv-test-3").
		WillReturnRows(sqlmock.NewRows([]string{"config", "version"}).AddRow(doctype.ToSExpr(&doctype.ConfigSnapshot{}), 3))
	mock.ExpectExec("INSERT INTO _kora_config_version").
		WithArgs(
			"cv-test-4", "test", 4, "system", "Activated", sqlmock.AnyArg(), "Active", true, sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), "cv-test-3", "",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	versionID, versionNum, err := s.CreateConfigVersionWithBase("test", "system", "Activated", "Active", snapshot, "cv-test-3")
	if err != nil {
		t.Fatalf("CreateConfigVersionWithBase error = %v", err)
	}
	if versionID != "cv-test-4" || versionNum != 4 {
		t.Fatalf("unexpected version result: id=%s version=%d", versionID, versionNum)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestConfigVersionDiffStoresFullSnapshotChanges(t *testing.T) {
	oldSnapshot := &doctype.ConfigSnapshot{Roles: []*doctype.Role{{Name: "Staff", WorkspaceAccess: true}}}
	newSnapshot := &doctype.ConfigSnapshot{
		Roles: []*doctype.Role{{Name: "Staff", WorkspaceAccess: true, Description: "Prepare records"}},
	}
	changeList, _, err := configVersionDiff(oldSnapshot, newSnapshot, 2, 3)
	if err != nil {
		t.Fatal("configVersionDiff:", err)
	}
	var diff doctype.ConfigDiffFull
	if err := json.Unmarshal([]byte(changeList), &diff); err != nil {
		t.Fatalf("stored change list is not a ConfigDiffFull: %v", err)
	}
	if diff.Doctypes == nil || len(diff.SectionChanges) != 1 {
		t.Fatalf("stored change list omitted full snapshot sections: %#v", diff)
	}
}

func TestConfigVersionIDBoundsLongSiteNames(t *testing.T) {
	short := configVersionID("test", 4)
	if short != "cv-test-4" {
		t.Fatalf("short site ID = %q, want cv-test-4", short)
	}

	long := configVersionID("cake_business_accounts.demo.local", 1)
	if len(long) > 36 {
		t.Fatalf("long site ID length = %d, want <= 36: %q", len(long), long)
	}
	if long == configVersionID("another.demo.local", 1) {
		t.Fatal("different site names must not share a compact version ID")
	}
}

func TestSupersedeSiblingDrafts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)
	mock.ExpectExec("UPDATE _kora_config_version SET status = 'Superseded'").
		WithArgs("test", "cv-test-5", "cv-test-2").
		WillReturnResult(sqlmock.NewResult(0, 2))

	if err := s.SupersedeSiblingDrafts("test", "cv-test-5", "cv-test-2"); err != nil {
		t.Fatalf("SupersedeSiblingDrafts error = %v", err)
	}
}

// Ensure the unused import compiles.
var _ = errors.New

func TestActivateSnapshotAtConfigRevisionUsesTransactionalCompareAndSet(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dialect := koraDB.Resolve("postgres")
	store := NewStore(database, dialect)
	authUpdate := `UPDATE _kora_auth_revision SET policy_revision = policy_revision + 1 WHERE id = 1`
	revisionUpdate := `UPDATE _kora_config_revision SET revision = revision + 1 WHERE id = 1 AND revision = $1 AND revision < 9223372036854775807`
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal("begin activation:", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(authUpdate)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(revisionUpdate)).WithArgs(int64(11)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.ActivateSnapshotAtConfigRevision(tx, &doctype.ConfigSnapshot{}, "tenant-a", dialect, 11); err != nil {
		t.Fatal("activate current revision:", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit activation:", err)
	}

	mock.ExpectBegin()
	tx, err = database.Begin()
	if err != nil {
		t.Fatal("begin stale activation:", err)
	}
	mock.ExpectExec(regexp.QuoteMeta(authUpdate)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(revisionUpdate)).WithArgs(int64(11)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.ActivateSnapshotAtConfigRevision(tx, &doctype.ConfigSnapshot{}, "tenant-a", dialect, 11); err != ErrConfigRevisionConflict {
		t.Fatalf("stale activation error = %v, want ErrConfigRevisionConflict", err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback stale activation:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestActivateSnapshotAtManagementClaimFencesBeforeConfigWrites(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dialect := koraDB.Resolve("postgres")
	store := NewStore(database, dialect)
	digest := strings.Repeat("a", 64)
	fenceQuery := "SELECT fence, expected_revision, artifact_digest FROM _kora_management_activation_fence WHERE site = $1 AND operation_id = $2 FOR UPDATE"
	authUpdate := "UPDATE _kora_auth_revision SET policy_revision = policy_revision + 1 WHERE id = 1"
	revisionUpdate := "UPDATE _kora_config_revision SET revision = revision + 1 WHERE id = 1 AND revision = $1 AND revision < 9223372036854775807"
	journalUpdate := "UPDATE _kora_config_operation SET phase = $1, recovery_plan = $2, last_error = '', updated_at = CURRENT_TIMESTAMP WHERE site = $3 AND version_id = $4 AND kind = $5 AND phase = $6"

	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal("begin current activation:", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(fenceQuery)).WithArgs("tenant-a", "operation-a").
		WillReturnRows(sqlmock.NewRows([]string{"fence", "expected_revision", "artifact_digest"}).AddRow(9, 12, digest))
	mock.ExpectExec(regexp.QuoteMeta(authUpdate)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(revisionUpdate)).WithArgs(int64(12)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(journalUpdate)).WithArgs(ActivationActivated, "recovery-plan", "tenant-a", "operation-a", "management_package", ActivationApplying).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.ActivateSnapshotAtManagementClaimWithJournal(context.Background(), tx, &doctype.ConfigSnapshot{}, "tenant-a", dialect, "operation-a", "management_package", ActivationApplying, "recovery-plan", 9, 12, digest); err != nil {
		t.Fatal("activate current management claim:", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit current activation:", err)
	}

	mock.ExpectBegin()
	tx, err = database.Begin()
	if err != nil {
		t.Fatal("begin stale activation:", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(fenceQuery)).WithArgs("tenant-a", "operation-a").
		WillReturnRows(sqlmock.NewRows([]string{"fence", "expected_revision", "artifact_digest"}).AddRow(10, 12, digest))
	if err := store.ActivateSnapshotAtManagementClaimWithJournal(context.Background(), tx, &doctype.ConfigSnapshot{}, "tenant-a", dialect, "operation-a", "management_package", ActivationApplying, "recovery-plan", 9, 12, digest); !errors.Is(err, ErrManagementActivationFence) {
		t.Fatalf("stale claim activation error = %v, want fence conflict", err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback stale activation:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
