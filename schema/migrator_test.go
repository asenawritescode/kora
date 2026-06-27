package schema

import (
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func newMySQLDialect() db.Dialect {
	return &db.MySQLDialect{}
}

func TestComputeDiff_NewTable(t *testing.T) {
	dialect := newMySQLDialect()
	reg := doctype.NewRegistry()
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
			{Fieldname: "status", Fieldtype: "Select", Label: "Status"},
		},
	}
	reg.Register(dt)

	// Empty live schema — everything is new.
	liveSchema := make(map[string]*TableInfo)

	diff := ComputeDiff(reg, liveSchema, dialect)
	if diff.IsEmpty() {
		t.Error("diff should not be empty for new doctype")
	}
	if len(diff.NewTables) != 1 {
		t.Errorf("NewTables = %v, want [tabTask]", diff.NewTables)
	}
	if diff.NewTables[0] != "tabTask" {
		t.Errorf("NewTables[0] = %q, want %q", diff.NewTables[0], "tabTask")
	}
}

func TestComputeDiff_AddNullableField(t *testing.T) {
	dialect := newMySQLDialect()

	// Registry has an additional field.
	reg := doctype.NewRegistry()
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
			{Fieldname: "description", Fieldtype: "Text", Label: "Description"},
		},
	}
	reg.Register(dt)

	// Live schema has the old version without 'description'.
	liveSchema := map[string]*TableInfo{
		"tabTask": {
			Name: "tabTask",
			Columns: map[string]*ColumnInfo{
				"name":  {Name: "name", Type: "VARCHAR(140)"},
				"owner": {Name: "owner", Type: "VARCHAR(140)"},
				"title": {Name: "title", Type: "VARCHAR(140)"},
			},
		},
	}

	diff := ComputeDiff(reg, liveSchema, dialect)
	if diff.IsEmpty() {
		t.Error("diff should not be empty")
	}
	if len(diff.NewTables) != 0 {
		t.Error("should not have new tables")
	}
	if len(diff.NewColumns["tabTask"]) != 1 {
		t.Fatalf("NewColumns[tabTask] = %v, want 1 column", diff.NewColumns["tabTask"])
	}
	if diff.NewColumns["tabTask"][0].Name != "description" {
		t.Errorf("column name = %q, want %q", diff.NewColumns["tabTask"][0].Name, "description")
	}
	if diff.NewColumns["tabTask"][0].Nullable != true {
		t.Error("new column should be nullable")
	}
}

func TestComputeDiff_DropField(t *testing.T) {
	dialect := newMySQLDialect()

	// Registry without a field that exists in the live schema.
	reg := doctype.NewRegistry()
	dt := &doctype.DocType{
		Name:   "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
		},
	}
	reg.Register(dt)

	liveSchema := map[string]*TableInfo{
		"tabTask": {
			Name: "tabTask",
			Columns: map[string]*ColumnInfo{
				"name":     {Name: "name", Type: "VARCHAR(140)"},
				"title":    {Name: "title", Type: "VARCHAR(140)"},
				"obsolete": {Name: "obsolete", Type: "VARCHAR(140)"},
			},
		},
	}

	diff := ComputeDiff(reg, liveSchema, dialect)
	if len(diff.Orphaned) != 1 {
		t.Fatalf("Orphaned = %v, want 1 orphaned column", diff.Orphaned)
	}
	if diff.Orphaned[0].Column != "obsolete" {
		t.Errorf("orphaned column = %q, want %q", diff.Orphaned[0].Column, "obsolete")
	}
	if diff.Orphaned[0].Table != "tabTask" {
		t.Errorf("orphaned table = %q, want %q", diff.Orphaned[0].Table, "tabTask")
	}
}

func TestComputeDiff_RenamedFrom(t *testing.T) {
	dialect := newMySQLDialect()

	// Registry field has renamed_from set.
	reg := doctype.NewRegistry()
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "full_name", Fieldtype: "Data", RenamedFrom: "old_name"},
		},
	}
	reg.Register(dt)

	// Live schema has the old column name.
	liveSchema := map[string]*TableInfo{
		"tabTask": {
			Name: "tabTask",
			Columns: map[string]*ColumnInfo{
				"name":     {Name: "name", Type: "VARCHAR(140)"},
				"old_name": {Name: "old_name", Type: "VARCHAR(140)"},
			},
		},
	}

	diff := ComputeDiff(reg, liveSchema, dialect)
	if len(diff.RenameColumns["tabTask"]) != 1 {
		t.Fatalf("RenameColumns = %v, want 1 rename", diff.RenameColumns["tabTask"])
	}
	if diff.RenameColumns["tabTask"][0].OldName != "old_name" {
		t.Errorf("OldName = %q, want %q", diff.RenameColumns["tabTask"][0].OldName, "old_name")
	}
	if diff.RenameColumns["tabTask"][0].NewName != "full_name" {
		t.Errorf("NewName = %q, want %q", diff.RenameColumns["tabTask"][0].NewName, "full_name")
	}
	if len(diff.NewColumns) != 0 {
		t.Error("should not have new columns when using renamed_from")
	}
}

func TestComputeDiff_NewIndex(t *testing.T) {
	dialect := newMySQLDialect()

	reg := doctype.NewRegistry()
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "email", Fieldtype: "Data", SearchIndex: true},
		},
	}
	reg.Register(dt)

	// Live schema has email column but no index on it.
	liveSchema := map[string]*TableInfo{
		"tabTask": {
			Name: "tabTask",
			Columns: map[string]*ColumnInfo{
				"name":  {Name: "name", Type: "VARCHAR(140)"},
				"email": {Name: "email", Type: "VARCHAR(140)", Indexed: false},
			},
		},
	}

	diff := ComputeDiff(reg, liveSchema, dialect)
	if len(diff.NewIndexes["tabTask"]) != 1 {
		t.Fatalf("NewIndexes = %v, want 1 index", diff.NewIndexes["tabTask"])
	}
	if diff.NewIndexes["tabTask"][0].Columns[0] != "email" {
		t.Errorf("index column = %q, want %q", diff.NewIndexes["tabTask"][0].Columns[0], "email")
	}
}

func TestAnalyzeImpact_SafeChanges(t *testing.T) {
	dialect := newMySQLDialect()
	reg := doctype.NewRegistry()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	oldDT := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
		},
	}
	reg.Register(oldDT)

	// Adding a nullable field with no default is safe.
	newDT := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
			{Fieldname: "description", Fieldtype: "Text"},
		},
	}

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabTask`").
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(5))

	preview := AnalyzeImpact(database, oldDT, newDT, reg, dialect)
	if len(preview.Blocked) != 0 {
		t.Errorf("Blocked = %v, want 0 blocked changes", preview.Blocked)
	}

	// Find the 'description' change.
	var found bool
	for _, c := range preview.Changes {
		if c.Field == "description" && c.Change == "Add field description (Text)" {
			found = true
			if c.Tier != "safe" {
				t.Errorf("description tier = %q, want %q", c.Tier, "safe")
			}
		}
	}
	if !found {
		t.Error("did not find 'description' field in changes")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAnalyzeImpact_WarningChanges(t *testing.T) {
	dialect := newMySQLDialect()
	reg := doctype.NewRegistry()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	oldDT := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
		},
	}
	reg.Register(oldDT)

	// Adding a required field WITHOUT a default is a warning.
	newDT := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
			{Fieldname: "mandatory_field", Fieldtype: "Data", Reqd: true},
		},
	}

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabTask`").
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(10))

	preview := AnalyzeImpact(database, oldDT, newDT, reg, dialect)
	var found bool
	for _, c := range preview.Changes {
		if c.Field == "mandatory_field" {
			found = true
			if c.Tier != "warning" {
				t.Errorf("mandatory_field tier = %q, want %q", c.Tier, "warning")
			}
		}
	}
	if !found {
		t.Error("did not find 'mandatory_field' in changes")
	}
	if len(preview.Warnings) == 0 {
		t.Error("should have at least 1 warning")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAnalyzeImpact_BlockedChanges(t *testing.T) {
	dialect := newMySQLDialect()
	reg := doctype.NewRegistry()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	oldDT := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "qty", Fieldtype: "Int"},
		},
	}
	reg.Register(oldDT)

	// Changing a field type is blocked.
	newDT := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "qty", Fieldtype: "Float"},
		},
	}

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabTask`").
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(3))

	preview := AnalyzeImpact(database, oldDT, newDT, reg, dialect)
	if len(preview.Blocked) == 0 {
		t.Error("should have at least 1 blocked change for type change")
	}
	for _, c := range preview.Blocked {
		if c.Field == "qty" && c.Change == "Change field type Int → Float" {
			if c.Tier != "blocked" {
				t.Errorf("tier = %q, want %q", c.Tier, "blocked")
			}
			return
		}
	}
	t.Error("did not find blocked 'qty' Int -> Float change")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestAnalyzeImpact_NewDoctype(t *testing.T) {
	dialect := newMySQLDialect()
	reg := doctype.NewRegistry()

	newDT := &doctype.DocType{
		Name: "NewEntity",
		Fields: []doctype.Field{
			{Fieldname: "name", Fieldtype: "Data"},
		},
	}
	reg.Register(newDT)

	// oldDT is nil — new doctype, always safe.
	preview := AnalyzeImpact(nil, nil, newDT, reg, dialect)
	if len(preview.Blocked) != 0 {
		t.Errorf("Blocked = %v, want 0 for new doctype", preview.Blocked)
	}
	if len(preview.Warnings) != 0 {
		t.Errorf("Warnings = %v, want 0 for new doctype", preview.Warnings)
	}
	if len(preview.DDL) == 0 {
		t.Error("DDL should not be empty for new doctype")
	}
}

func TestGenerateDDL_CreateTable(t *testing.T) {
	dialect := newMySQLDialect()
	reg := doctype.NewRegistry()
	dt := &doctype.DocType{
		Name: "NewDoc",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
		},
	}
	reg.Register(dt)

	diff := &Diff{
		NewTables: []string{"tabNewDoc"},
	}

	ddl := diff.GenerateDDL(reg, dialect)
	if len(ddl) == 0 {
		t.Fatal("GenerateDDL returned empty statements")
	}
	if !contains(ddl[0], "CREATE TABLE") {
		t.Errorf("first DDL = %q..., want CREATE TABLE", ddl[0][:20])
	}
	if !contains(ddl[0], "`tabNewDoc`") {
		t.Errorf("first DDL should contain tabNewDoc, got: %s", ddl[0])
	}
}

func TestGenerateDDL_AddColumn(t *testing.T) {
	dialect := newMySQLDialect()
	reg := doctype.NewRegistry()
	dt := &doctype.DocType{
		Name: "ExistingDoc",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
		},
	}
	reg.Register(dt)

	diff := &Diff{
		NewColumns: map[string][]ColumnAdd{
			"tabExistingDoc": {
				{Name: "new_col", Type: "VARCHAR(140)", Nullable: true},
			},
		},
	}

	ddl := diff.GenerateDDL(reg, dialect)
	if len(ddl) == 0 {
		t.Fatal("GenerateDDL returned empty statements")
	}
	if !contains(ddl[0], "ALTER TABLE") {
		t.Errorf("first DDL = %q..., want ALTER TABLE", ddl[0][:15])
	}
	if !contains(ddl[0], "ADD COLUMN") {
		t.Errorf("should contain ADD COLUMN, got: %s", ddl[0])
	}
	if !contains(ddl[0], "new_col") {
		t.Errorf("should contain new_col, got: %s", ddl[0])
	}
}

func TestDiffIsEmpty(t *testing.T) {
	tests := []struct {
		name  string
		diff  *Diff
		empty bool
	}{
		{
			name:  "empty diff",
			diff:  &Diff{},
			empty: true,
		},
		{
			name:  "new tables",
			diff:  &Diff{NewTables: []string{"tabX"}},
			empty: false,
		},
		{
			name:  "new columns",
			diff:  &Diff{NewColumns: map[string][]ColumnAdd{"tabX": {{Name: "c"}}}},
			empty: false,
		},
		{
			name:  "new indexes",
			diff:  &Diff{NewIndexes: map[string][]IndexAdd{"tabX": {{Columns: []string{"c"}}}}},
			empty: false,
		},
		{
			name:  "renamed columns",
			diff:  &Diff{RenameColumns: map[string][]ColumnRename{"tabX": {{OldName: "a", NewName: "b"}}}},
			empty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.diff.IsEmpty()
			if got != tt.empty {
				t.Errorf("IsEmpty() = %v, want %v", got, tt.empty)
			}
		})
	}
}

// contains reports whether substr is in s.
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure the package compiles without unused import errors.
var _ = sql.Open
