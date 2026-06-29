package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/lib/pq"
)

func TestPostgres_ColumnType(t *testing.T) {
	d := &PostgresDialect{}

	tests := []struct {
		name     string
		field    doctype.Field
		expected string
	}{
		{"Data", doctype.Field{Fieldtype: "Data"}, "VARCHAR(140)"},
		{"Int", doctype.Field{Fieldtype: "Int"}, "BIGINT"},
		{"Float", doctype.Field{Fieldtype: "Float"}, "DECIMAL(21,9)"},
		{"Currency", doctype.Field{Fieldtype: "Currency"}, "DECIMAL(21,9)"},
		{"Percent", doctype.Field{Fieldtype: "Percent"}, "DECIMAL(21,9)"},
		{"Check", doctype.Field{Fieldtype: "Check"}, "BOOLEAN"},
		{"Date", doctype.Field{Fieldtype: "Date"}, "DATE"},
		{"Time", doctype.Field{Fieldtype: "Time"}, "TIME"},
		{"Datetime", doctype.Field{Fieldtype: "Datetime"}, "TIMESTAMP"},
		{"Text", doctype.Field{Fieldtype: "Text"}, "TEXT"},
		{"Text Editor", doctype.Field{Fieldtype: "Text Editor"}, "TEXT"},
		{"Select", doctype.Field{Fieldtype: "Select"}, "VARCHAR(140)"},
		{"Link", doctype.Field{Fieldtype: "Link"}, "VARCHAR(140)"},
		{"Dynamic Link", doctype.Field{Fieldtype: "Dynamic Link"}, "VARCHAR(140)"},
		{"Attach", doctype.Field{Fieldtype: "Attach"}, "TEXT"},
		{"Attach Image", doctype.Field{Fieldtype: "Attach Image"}, "TEXT"},
		{"JSON", doctype.Field{Fieldtype: "JSON"}, "JSONB"},
		{"Password", doctype.Field{Fieldtype: "Password"}, "VARCHAR(255)"},
		{"Unknown", doctype.Field{Fieldtype: "Unknown"}, "TEXT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.ColumnType(&tt.field)
			if got != tt.expected {
				t.Errorf("Postgres ColumnType(%s) = %q, want %q", tt.name, got, tt.expected)
			}
		})
	}
}

func TestPostgres_CreateTable(t *testing.T) {
	d := &PostgresDialect{}
	dt := &doctype.DocType{
		Name: "Test DocType",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
			{Fieldname: "qty", Fieldtype: "Int", Label: "Quantity"},
			{Fieldname: "amount", Fieldtype: "Currency", Label: "Amount", Reqd: true},
		},
	}

	stmts := d.CreateTable(dt)
	if len(stmts) == 0 {
		t.Fatal("CreateTable returned no statements")
	}

	first := stmts[0]
	if !strings.HasPrefix(first, "CREATE TABLE") {
		t.Errorf("first statement = %q..., want CREATE TABLE", first[:12])
	}
	if !contains(first, `"title" VARCHAR(140)`) {
		t.Error("CreateTable should include title column")
	}
	if !contains(first, `"qty" BIGINT`) {
		t.Error("CreateTable should include qty column")
	}
	if !contains(first, `"amount" DECIMAL(21,9) NOT NULL`) {
		t.Error("CreateTable should include amount as NOT NULL")
	}
	if !contains(first, `PRIMARY KEY`) {
		t.Error("CreateTable should include PRIMARY KEY")
	}
	// PostgreSQL should NOT have ENGINE clause.
	if contains(first, "ENGINE") {
		t.Error("CreateTable should not include ENGINE clause")
	}
}

func TestPostgres_CreateTableWithIndexes(t *testing.T) {
	d := &PostgresDialect{}
	dt := &doctype.DocType{
		Name: "Test DocType",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "sku", Fieldtype: "Data", Unique: true},
		},
	}

	stmts := d.CreateTable(dt)
	if len(stmts) < 3 {
		t.Fatalf("expected at least 3 statements (CREATE TABLE + 2 indexes), got %d", len(stmts))
	}

	foundSearchIndex := false
	foundUniqueIndex := false
	for _, s := range stmts[1:] {
		if strings.Contains(s, "CREATE INDEX") && strings.Contains(s, "title") {
			foundSearchIndex = true
		}
		if strings.Contains(s, "CREATE UNIQUE INDEX") && strings.Contains(s, "sku") {
			foundUniqueIndex = true
		}
	}
	if !foundSearchIndex {
		t.Error("expected search index for title field")
	}
	if !foundUniqueIndex {
		t.Error("expected UNIQUE index for sku field")
	}
}

func TestPostgres_AddColumn(t *testing.T) {
	d := &PostgresDialect{}

	tests := []struct {
		name  string
		field doctype.Field
	}{
		{
			name:  "nullable data field",
			field: doctype.Field{Fieldname: "email", Fieldtype: "Data"},
		},
		{
			name:  "required int field",
			field: doctype.Field{Fieldname: "count", Fieldtype: "Int", Reqd: true},
		},
		{
			name:  "field with default",
			field: doctype.Field{Fieldname: "status", Fieldtype: "Data", Default: "active"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt := d.AddColumn("tabTest", &tt.field)
			if stmt == "" {
				t.Error("AddColumn returned empty statement")
			}
			if !contains(stmt, "ALTER TABLE") {
				t.Error("AddColumn should start with ALTER TABLE")
			}
			if !contains(stmt, "ADD COLUMN") {
				t.Error("AddColumn should contain ADD COLUMN")
			}
			if !contains(stmt, d.QuoteIdent(tt.field.Fieldname)) {
				t.Error("AddColumn should include quoted field name")
			}
		})
	}
}

func TestPostgres_AlterColumn(t *testing.T) {
	d := &PostgresDialect{}
	stmt := d.AlterColumn("tabProduct", &doctype.Field{Fieldname: "price", Fieldtype: "Currency"})

	if !contains(stmt, "ALTER TABLE") {
		t.Error("AlterColumn should contain ALTER TABLE")
	}
	if !contains(stmt, "ALTER COLUMN") {
		t.Error("AlterColumn should contain ALTER COLUMN")
	}
	if !contains(stmt, "TYPE") {
		t.Error("AlterColumn should contain TYPE")
	}
	if !contains(stmt, "DECIMAL") {
		t.Error("AlterColumn should include the new column type")
	}
}

func TestPostgres_RenameColumn(t *testing.T) {
	d := &PostgresDialect{}
	stmt := d.RenameColumn("tabProduct", "old_name", "new_name")

	if !contains(stmt, "RENAME COLUMN") {
		t.Errorf("expected RENAME COLUMN, got: %s", stmt)
	}
	if !contains(stmt, "old_name") {
		t.Errorf("expected old_name, got: %s", stmt)
	}
	if !contains(stmt, "new_name") {
		t.Errorf("expected new_name, got: %s", stmt)
	}
}

func TestPostgres_DropColumn(t *testing.T) {
	d := &PostgresDialect{}
	stmt := d.DropColumn("tabProduct", "obsolete")

	if !contains(stmt, "DROP COLUMN") {
		t.Errorf("expected DROP COLUMN, got: %s", stmt)
	}
	if !contains(stmt, "obsolete") {
		t.Errorf("expected column name, got: %s", stmt)
	}
}

func TestPostgres_CreateIndex(t *testing.T) {
	d := &PostgresDialect{}

	t.Run("unique index", func(t *testing.T) {
		stmt := d.CreateIndex("tabProduct", "sku", true)
		if !contains(stmt, "CREATE UNIQUE INDEX") {
			t.Errorf("expected CREATE UNIQUE INDEX, got: %s", stmt)
		}
		if !contains(stmt, "IF NOT EXISTS") {
			t.Errorf("expected IF NOT EXISTS, got: %s", stmt)
		}
	})

	t.Run("non-unique index", func(t *testing.T) {
		stmt := d.CreateIndex("tabProduct", "title", false)
		if !contains(stmt, "CREATE INDEX") && contains(stmt, "UNIQUE") {
			t.Errorf("expected non-UNIQUE CREATE INDEX, got: %s", stmt)
		}
		if !contains(stmt, "IF NOT EXISTS") {
			t.Errorf("expected IF NOT EXISTS, got: %s", stmt)
		}
	})
}

func TestPostgres_DropIndex(t *testing.T) {
	d := &PostgresDialect{}
	stmt := d.DropIndex("tabProduct", "idx_tabProduct_title")

	if !contains(stmt, "DROP INDEX") {
		t.Errorf("expected DROP INDEX, got: %s", stmt)
	}
	if !contains(stmt, "IF EXISTS") {
		t.Errorf("expected IF EXISTS, got: %s", stmt)
	}
	// PostgreSQL DROP INDEX should NOT have ON clause.
	if contains(stmt, "ON") {
		t.Errorf("DROP INDEX should not have ON clause, got: %s", stmt)
	}
}

func TestPostgres_UpsertClause(t *testing.T) {
	d := &PostgresDialect{}

	tests := []struct {
		name     string
		conflict []string
		update   []string
		want     string
	}{
		{
			name:     "single column",
			conflict: []string{"name"},
			update:   []string{"email", "status"},
			want:     `ON CONFLICT("name") DO UPDATE SET "email" = EXCLUDED."email", "status" = EXCLUDED."status"`,
		},
		{
			name:     "multiple conflict columns",
			conflict: []string{"site", "email"},
			update:   []string{"full_name"},
			want:     `ON CONFLICT("site", "email") DO UPDATE SET "full_name" = EXCLUDED."full_name"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.UpsertClause(tt.conflict, tt.update)
			if got != tt.want {
				t.Errorf("UpsertClause = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPostgres_UpsertIncrement(t *testing.T) {
	d := &PostgresDialect{}

	tests := []struct {
		name     string
		conflict []string
		incCols  []string
		want     string
	}{
		{
			name:     "single increment column",
			conflict: []string{"site", "date"},
			incCols:  []string{"count"},
			want:     `ON CONFLICT("site", "date") DO UPDATE SET "count" = "count" + EXCLUDED."count"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.UpsertIncrement(tt.conflict, tt.incCols)
			if got != tt.want {
				t.Errorf("UpsertIncrement = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPostgres_InsertOrIgnorePrefix(t *testing.T) {
	d := &PostgresDialect{}
	got := d.InsertOrIgnorePrefix()
	if got != "INSERT" {
		t.Errorf("InsertOrIgnorePrefix = %q, want %q", got, "INSERT")
	}
}

func TestPostgres_Placeholder(t *testing.T) {
	d := &PostgresDialect{}

	tests := []struct {
		n    int
		want string
	}{
		{1, "$1"},
		{2, "$2"},
		{10, "$10"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("n=%d", tt.n), func(t *testing.T) {
			got := d.Placeholder(tt.n)
			if got != tt.want {
				t.Errorf("Placeholder(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestPostgres_NameGenQuery(t *testing.T) {
	d := &PostgresDialect{}
	got := d.NameGenQuery("tabTask", "TASK")
	expected := `SELECT COALESCE(MAX(CAST(SPLIT_PART(name, '-', 2) AS INTEGER)), 0) FROM "tabTask" WHERE name LIKE 'TASK-%'`
	if got != expected {
		t.Errorf("NameGenQuery = %q, want %q", got, expected)
	}
}

func TestPostgres_QuoteIdent(t *testing.T) {
	d := &PostgresDialect{}

	tests := []struct {
		input string
		want  string
	}{
		{"tabUser", `"tabUser"`},
		{"tabWork Order", `"tabWork Order"`},
		{"name", `"name"`},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := d.QuoteIdent(tt.input)
			if got != tt.want {
				t.Errorf("QuoteIdent(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPostgres_NowTimestamp(t *testing.T) {
	d := &PostgresDialect{}
	got := d.NowTimestamp()
	if got != "NOW()" {
		t.Errorf("NowTimestamp = %q, want %q", got, "NOW()")
	}
}

func TestPostgres_TableSuffix(t *testing.T) {
	d := &PostgresDialect{}
	got := d.TableSuffix()
	if got != "" {
		t.Errorf("TableSuffix = %q, want empty string", got)
	}
}

func TestPostgres_DriverName(t *testing.T) {
	d := &PostgresDialect{}
	if got := d.DriverName(); got != "postgres" {
		t.Errorf("DriverName = %q, want %q", got, "postgres")
	}
}

func TestPostgres_SystemColumnDDL(t *testing.T) {
	d := &PostgresDialect{}
	cols := d.SystemColumnDDL()
	if len(cols) != 7 {
		t.Fatalf("expected 7 system columns, got %d", len(cols))
	}
	// Check for double-quoted identifiers (PostgreSQL style).
	for _, c := range cols {
		if !strings.HasPrefix(c, `"`) {
			t.Errorf("system column should start with double quote: %s", c)
		}
	}
}

func TestPostgres_ChildColumnDDL(t *testing.T) {
	d := &PostgresDialect{}
	cols := d.ChildColumnDDL()
	if len(cols) != 3 {
		t.Fatalf("expected 3 child columns, got %d", len(cols))
	}
	if !contains(cols[0], "parent") {
		t.Errorf("expected parent column, got: %s", cols[0])
	}
	if !contains(cols[1], "parentfield") {
		t.Errorf("expected parentfield column, got: %s", cols[1])
	}
	if !contains(cols[2], "parenttype") {
		t.Errorf("expected parenttype column, got: %s", cols[2])
	}
}

func TestPostgres_ParseError_Unique(t *testing.T) {
	d := &PostgresDialect{}
	dt := &doctype.DocType{
		Name: "User",
		Fields: []doctype.Field{
			{Fieldname: "email", Label: "Email"},
		},
	}

	// Simulate a pq unique violation error.
	pqErr := &pq.Error{
		Severity: "ERROR",
		Code:     "23505",
		Message:  "duplicate key value violates unique constraint \"uq_tabUser_email\"",
		Column:   "email",
		Table:    "tabUser",
	}

	ve := d.ParseError(pqErr, dt)
	if ve == nil {
		t.Fatal("ParseError should return a ValidationError for duplicate key")
	}
	if ve.Type != "UniqueConstraint" {
		t.Errorf("Type = %q, want %q", ve.Type, "UniqueConstraint")
	}
	if ve.Field != "email" {
		t.Errorf("Field = %q, want %q", ve.Field, "email")
	}
}

func TestPostgres_ParseError_NotNull(t *testing.T) {
	d := &PostgresDialect{}
	dt := &doctype.DocType{
		Name: "User",
		Fields: []doctype.Field{
			{Fieldname: "full_name", Label: "Full Name"},
		},
	}

	pqErr := &pq.Error{
		Severity: "ERROR",
		Code:     "23502",
		Message:  "null value in column \"full_name\" violates not-null constraint",
		Column:   "full_name",
		Table:    "tabUser",
	}

	ve := d.ParseError(pqErr, dt)
	if ve == nil {
		t.Fatal("ParseError should return a ValidationError for not null")
	}
	if ve.Type != "NotNullConstraint" {
		t.Errorf("Type = %q, want %q", ve.Type, "NotNullConstraint")
	}
	if ve.Message != "Full Name is required." {
		t.Errorf("Message = %q, want %q", ve.Message, "Full Name is required.")
	}
}

func TestPostgres_ParseError_Unrelated(t *testing.T) {
	d := &PostgresDialect{}
	dt := &doctype.DocType{Name: "Test"}

	// Non-pq error.
	ve := d.ParseError(errors.New("connection refused"), dt)
	if ve != nil {
		t.Error("ParseError should return nil for non-pq errors")
	}

	// Non-constraint pq error.
	pqErr := &pq.Error{
		Severity: "FATAL",
		Code:     "28000",
		Message:  "no pg_hba.conf entry",
	}
	ve = d.ParseError(pqErr, dt)
	if ve != nil {
		t.Error("ParseError should return nil for non-constraint errors")
	}
}

func TestPostgres_SystemTableSQL(t *testing.T) {
	d := &PostgresDialect{}
	stmts := d.SystemTableSQL()

	if len(stmts) == 0 {
		t.Fatal("SystemTableSQL returned no statements")
	}

	// Check that all CREATE TABLE statements use PostgreSQL-compatible syntax.
	for _, stmt := range stmts {
		if strings.HasPrefix(stmt, "CREATE TABLE") {
			if contains(stmt, "TINYINT") {
				t.Errorf("PostgreSQL should not use TINYINT: %s", stmt)
			}
			if contains(stmt, "ENGINE") {
				t.Errorf("PostgreSQL should not use ENGINE clause: %s", stmt)
			}
			if contains(stmt, "`") {
				t.Errorf("PostgreSQL should not use backtick quoting: %s", stmt)
			}
		}
	}
}

func TestPostgres_Resolve(t *testing.T) {
	d := Resolve("postgres")
	if _, ok := d.(*PostgresDialect); !ok {
		t.Errorf("Resolve('postgres') returned %T, want *PostgresDialect", d)
	}
}

func TestPostgres_SystemTablesCreateAll(t *testing.T) {
	d := &PostgresDialect{}
	stmts := d.SystemTableSQL()

	expectedTables := []string{
		"_kora_doctype",
		"_kora_field",
		"_kora_role",
		"_kora_permission",
		"_kora_config_version",
		"_kora_user",
		"_kora_session",
		"_kora_workflow",
		"_kora_workflow_state",
		"_kora_workflow_transition",
		"_kora_secret",
	}

	for _, table := range expectedTables {
		found := false
		for _, stmt := range stmts {
			if strings.HasPrefix(stmt, "CREATE TABLE") && contains(stmt, `"`+table+`"`) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SystemTableSQL missing CREATE TABLE for %s", table)
		}
	}
}
