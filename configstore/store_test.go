package configstore

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
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
	mock.ExpectQuery("SELECT name, module, is_submittable, is_child_table, is_single, track_changes, title_field, search_fields, sort_field, sort_order, description FROM _kora_doctype ORDER BY name").
		WillReturnRows(sqlmock.NewRows([]string{"name", "module", "is_submittable", "is_child_table", "is_single", "track_changes", "title_field", "search_fields", "sort_field", "sort_order", "description"}))

	// Expect field query — also empty.
	mock.ExpectQuery("SELECT parent, fieldname, fieldtype, label, options, reqd, unique_constraint, default_value, hidden, read_only, bold, in_list_view, in_standard_filter, search_index, description, depends_on, mandatory_depends_on, constraints_json, renamed_from, COALESCE\\(linked_field,''\\).*FROM _kora_field.*ORDER BY parent, idx").
		WillReturnRows(sqlmock.NewRows([]string{
			"parent", "fieldname", "fieldtype", "label", "options",
			"reqd", "unique_constraint", "default_value", "hidden", "read_only",
			"bold", "in_list_view", "in_standard_filter", "search_index",
			"description", "depends_on", "mandatory_depends_on", "constraints_json",
			"renamed_from", "linked_field", "computed", "idx",
		}))

	doctypes, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll error = %v", err)
	}
	if len(doctypes) != 0 {
		t.Errorf("LoadAll returned %d doctypes, want 0", len(doctypes))
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
	dtRows := sqlmock.NewRows([]string{"name", "module", "is_submittable", "is_child_table", "is_single", "track_changes", "title_field", "search_fields", "sort_field", "sort_order", "description"}).
		AddRow("Task", "Core", 1, 0, 0, 0, "subject", "subject,status", "modified", "DESC", "A task").
		AddRow("User", "Core", 0, 0, 0, 0, "name", "name,email", "modified", "DESC", "A user")

	mock.ExpectQuery("SELECT name, module, is_submittable, is_child_table, is_single, track_changes, title_field, search_fields, sort_field, sort_order, description FROM _kora_doctype ORDER BY name").
		WillReturnRows(dtRows)

	// Field rows.
	fieldRows := sqlmock.NewRows([]string{
		"parent", "fieldname", "fieldtype", "label", "options",
		"reqd", "unique_constraint", "default_value", "hidden", "read_only",
		"bold", "in_list_view", "in_standard_filter", "search_index",
		"description", "depends_on", "mandatory_depends_on", "constraints_json",
		"renamed_from", "linked_field", "computed", "idx",
	}).
		AddRow("Task", "subject", "Data", "Subject", "", 1, 0, "", 0, 0, 0, 1, 0, 0, "", "", "", "[]", "", "", "", 0).
		AddRow("Task", "status", "Select", "Status", "Open,Closed", 1, 0, "Open", 0, 0, 0, 1, 0, 0, "", "", "", "[]", "", "", "", 1).
		AddRow("User", "email", "Data", "Email", "", 1, 1, "", 0, 0, 0, 1, 0, 0, "", "", "", "[]", "", "", "", 0)

	mock.ExpectQuery("SELECT parent, fieldname, fieldtype, label, options, reqd, unique_constraint, default_value, hidden, read_only, bold, in_list_view, in_standard_filter, search_index, description, depends_on, mandatory_depends_on, constraints_json, renamed_from, COALESCE\\(linked_field,''\\).*FROM _kora_field.*ORDER BY parent, idx").
		WillReturnRows(fieldRows)

	doctypes, err := s.LoadAll()
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

	for _, role := range roles {
		expectUpsert := `INSERT INTO _kora_role \(name, workspace_access, description\) VALUES \(\?, \?, \?\) ON DUPLICATE KEY UPDATE`
		mock.ExpectExec(expectUpsert).
			WithArgs(role.Name, 1, role.Description).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}

	err = s.SaveRoles(roles)
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
		"can_report", "if_owner",
	}).
		AddRow("Task", "Admin", 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0).
		AddRow("Task", "Editor", 1, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0).
		AddRow("User", "Admin", 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0)

	mock.ExpectQuery("SELECT doctype, role, can_read, can_write, can_create, can_delete, can_submit, can_cancel, can_amend, can_export, can_import, can_report, if_owner FROM _kora_permission ORDER BY doctype, role").
		WillReturnRows(rows)

	perms, err := s.LoadPermissions()
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
	if perms[1].Delete {
		t.Error("Editor/Task should NOT have delete")
	}
}

func TestLoadWorkflows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	s := newStore(db)

	configJSON := `{"name":"Task Workflow","document_type":"Task","is_active":true,"states":[{"state":"Draft","doc_status":0}],"transitions":[{"action":"Submit","from":"Draft","to":"Approved"}]}`

	rows := sqlmock.NewRows([]string{"config_json"}).
		AddRow(configJSON)

	mock.ExpectQuery("SELECT config_json FROM _kora_workflow WHERE is_active = 1").
		WillReturnRows(rows)

	workflows, err := s.LoadWorkflows()
	if err != nil {
		t.Fatalf("LoadWorkflows error = %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("LoadWorkflows returned %d workflows, want 1", len(workflows))
	}
	if workflows[0].Name != "Task Workflow" {
		t.Errorf("workflow name = %q, want %q", workflows[0].Name, "Task Workflow")
	}
	if workflows[0].DocumentType != "Task" {
		t.Errorf("document_type = %q, want %q", workflows[0].DocumentType, "Task")
	}
	if !workflows[0].IsActive {
		t.Error("workflow should be active")
	}
	if len(workflows[0].States) != 1 {
		t.Errorf("states = %d, want 1", len(workflows[0].States))
	}
	if len(workflows[0].Transitions) != 1 {
		t.Errorf("transitions = %d, want 1", len(workflows[0].Transitions))
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
		"workflow_action", "schedule", "priority", "is_active", "run_as",
		"timeout_ms", "script",
	}).
		AddRow("validate_task", "doc_event", "Task", "before_save", "", "", "", 10, 1, "", 5000, "console.log('validating')").
		AddRow("daily_report", "scheduled", "", "", "", "", "0 9 * * *", 5, 1, "Admin", 30000, "console.log('report')")

	mock.ExpectQuery("SELECT name, script_type, doctype, event, method_path, workflow_action, schedule, priority, is_active, run_as, timeout_ms, script FROM _kora_script WHERE is_active = 1").
		WillReturnRows(rows)

	snapshots, err := s.LoadScriptSnapshots()
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

	mock.ExpectQuery("SELECT name, workspace_access, description FROM _kora_role ORDER BY name").
		WillReturnRows(rows)

	roles, err := s.LoadRoles()
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
			Doctype: "Task",
			Role:    "Guest",
			Read:    true,
		},
	}

	mock.ExpectExec("INSERT INTO _kora_permission").
		WithArgs("Task.Admin", "Task", "Admin", 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec("INSERT INTO _kora_permission").
		WithArgs("Task.Guest", "Task", "Guest", 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = s.SavePermissions(perms)
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

// Ensure the unused import compiles.
var _ = errors.New
