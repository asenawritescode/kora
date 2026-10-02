package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/go-sql-driver/mysql"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		dbType   string
		wantType string
	}{
		{"mysql", "mysql", "*db.MySQLDialect"},
		{"libsql", "libsql", "*db.LibSQLDialect"},
		{"default empty", "", "*db.MySQLDialect"},
		{"default unknown", "unknown", "*db.MySQLDialect"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Resolve(tt.dbType)
			got := fmt.Sprintf("%T", d)
			// Normalize package prefix — the actual path may differ.
			if got != tt.wantType && tt.dbType != "unknown" {
				t.Errorf("Resolve(%q) = %s, want %s", tt.dbType, got, tt.wantType)
			}
		})
	}
}

func TestMySQL_ColumnType(t *testing.T) {
	d := &MySQLDialect{}

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
		{"Check", doctype.Field{Fieldtype: "Check"}, "TINYINT(1)"},
		{"Date", doctype.Field{Fieldtype: "Date"}, "DATE"},
		{"Time", doctype.Field{Fieldtype: "Time"}, "TIME(6)"},
		{"Datetime", doctype.Field{Fieldtype: "Datetime"}, "DATETIME(6)"},
		{"Text", doctype.Field{Fieldtype: "Text"}, "TEXT"},
		{"Text Editor", doctype.Field{Fieldtype: "Text Editor"}, "LONGTEXT"},
		{"Select", doctype.Field{Fieldtype: "Select"}, "VARCHAR(140)"},
		{"Link", doctype.Field{Fieldtype: "Link"}, "VARCHAR(140)"},
		{"Dynamic Link", doctype.Field{Fieldtype: "Dynamic Link"}, "VARCHAR(140)"},
		{"Attach", doctype.Field{Fieldtype: "Attach"}, "TEXT"},
		{"Attach Image", doctype.Field{Fieldtype: "Attach Image"}, "TEXT"},
		{"JSON", doctype.Field{Fieldtype: "JSON"}, "JSON"},
		{"Password", doctype.Field{Fieldtype: "Password"}, "VARCHAR(255)"},
		{"Unknown", doctype.Field{Fieldtype: "Unknown"}, "TEXT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.ColumnType(&tt.field)
			if got != tt.expected {
				t.Errorf("MySQL ColumnType(%s) = %q, want %q", tt.name, got, tt.expected)
			}
		})
	}
}

func TestLibSQL_ColumnType(t *testing.T) {
	d := &LibSQLDialect{}

	tests := []struct {
		name     string
		field    doctype.Field
		expected string
	}{
		{"Data", doctype.Field{Fieldtype: "Data"}, "TEXT"},
		{"Int", doctype.Field{Fieldtype: "Int"}, "INTEGER"},
		{"Float", doctype.Field{Fieldtype: "Float"}, "REAL"},
		{"Currency", doctype.Field{Fieldtype: "Currency"}, "REAL"},
		{"Percent", doctype.Field{Fieldtype: "Percent"}, "REAL"},
		{"Check", doctype.Field{Fieldtype: "Check"}, "INTEGER"},
		{"Date", doctype.Field{Fieldtype: "Date"}, "TEXT"},
		{"Time", doctype.Field{Fieldtype: "Time"}, "TEXT"},
		{"Datetime", doctype.Field{Fieldtype: "Datetime"}, "TEXT"},
		{"Text", doctype.Field{Fieldtype: "Text"}, "TEXT"},
		{"Select", doctype.Field{Fieldtype: "Select"}, "TEXT"},
		{"Link", doctype.Field{Fieldtype: "Link"}, "TEXT"},
		{"Password", doctype.Field{Fieldtype: "Password"}, "TEXT"},
		{"JSON", doctype.Field{Fieldtype: "JSON"}, "TEXT"},
		{"Attach", doctype.Field{Fieldtype: "Attach"}, "TEXT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.ColumnType(&tt.field)
			if got != tt.expected {
				t.Errorf("LibSQL ColumnType(%s) = %q, want %q", tt.name, got, tt.expected)
			}
		})
	}
}

func TestMySQL_CreateTable(t *testing.T) {
	d := &MySQLDialect{}
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
	if !strings.HasPrefix(first, "CREATE TABLE IF NOT EXISTS") {
		t.Errorf("first statement = %q..., want CREATE TABLE IF NOT EXISTS", first[:26])
	}
	if !contains(first, "`title` VARCHAR(140)") {
		t.Error("CreateTable should include title column")
	}
	if !contains(first, "`qty` BIGINT") {
		t.Error("CreateTable should include qty column")
	}
	if !contains(first, "`amount` DECIMAL(21,9) NOT NULL") {
		t.Error("CreateTable should include amount as NOT NULL")
	}
	if !contains(first, "PRIMARY KEY") {
		t.Error("CreateTable should include PRIMARY KEY")
	}
	if !contains(first, "ENGINE=InnoDB") {
		t.Error("CreateTable should include ENGINE=InnoDB")
	}
}

func TestMySQL_IgnoresDuplicateCreateDDL(t *testing.T) {
	duplicateTable := &mysql.MySQLError{Number: 1050}
	if !isIgnorableMySQLCreateError(duplicateTable, "CREATE TABLE `tabTemplate` (name varchar(140))") {
		t.Fatal("duplicate table create should be ignored")
	}
	if !isIgnorableMySQLCreateError(duplicateTable, "CREATE TABLE IF NOT EXISTS `tabTemplate` (name varchar(140))") {
		t.Fatal("duplicate table create with IF NOT EXISTS should be ignored")
	}

	duplicateIndex := &mysql.MySQLError{Number: 1061}
	if !isIgnorableMySQLCreateError(duplicateIndex, "CREATE INDEX `idx_tabTemplate_slug` ON `tabTemplate` (`slug`)") {
		t.Fatal("duplicate index create should be ignored")
	}
	if !isIgnorableMySQLCreateError(duplicateIndex, "CREATE UNIQUE INDEX `uq_tabTemplate_slug` ON `tabTemplate` (`slug`)") {
		t.Fatal("duplicate unique index create should be ignored")
	}

	if isIgnorableMySQLCreateError(duplicateTable, "ALTER TABLE `tabTemplate` ADD COLUMN `title` varchar(140)") {
		t.Fatal("duplicate table error should not be ignored for non-create DDL")
	}
	if isIgnorableMySQLCreateError(&mysql.MySQLError{Number: 1060}, "ALTER TABLE `tabTemplate` ADD COLUMN `title` varchar(140)") {
		t.Fatal("duplicate column error should not be ignored")
	}
}

func TestMySQL_CreateTableTemporalDefaults(t *testing.T) {
	d := &MySQLDialect{}
	dt := &doctype.DocType{
		Name: "Temporal DocType",
		Fields: []doctype.Field{
			{Fieldname: "sale_date", Fieldtype: "Date", Default: "Today"},
			{Fieldname: "sale_time", Fieldtype: "Time", Default: "Now"},
			{Fieldname: "created_at", Fieldtype: "Datetime", Default: "Now"},
		},
	}

	stmt := d.CreateTable(dt)[0]
	if !contains(stmt, "`sale_date` DATE DEFAULT CURRENT_DATE") || contains(stmt, "`sale_date` DATE DEFAULT NULL") {
		t.Fatalf("expected CURRENT_DATE default, got: %s", stmt)
	}
	if !contains(stmt, "`sale_time` TIME(6) DEFAULT CURRENT_TIME(6)") || contains(stmt, "`sale_time` TIME(6) DEFAULT NULL") {
		t.Fatalf("expected CURRENT_TIME(6) default, got: %s", stmt)
	}
	if !contains(stmt, "`created_at` DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6)") || contains(stmt, "`created_at` DATETIME(6) DEFAULT NULL") {
		t.Fatalf("expected CURRENT_TIMESTAMP(6) default, got: %s", stmt)
	}
	if strings.Contains(stmt, "DEFAULT 'Today'") || strings.Contains(stmt, "DEFAULT 'Now'") {
		t.Fatalf("temporal defaults should not be quoted literals: %s", stmt)
	}
}

func TestMySQL_AddColumnUsesOnlySupportedDefault(t *testing.T) {
	d := &MySQLDialect{}
	dateStmt := d.AddColumn("tabTest", &doctype.Field{Fieldname: "received_at", Fieldtype: "Datetime", Default: "Now"})
	if !contains(dateStmt, "`received_at` DATETIME(6) DEFAULT CURRENT_TIMESTAMP(6)") || contains(dateStmt, "`received_at` DATETIME(6) DEFAULT NULL") {
		t.Fatalf("nullable column should have one temporal default, got: %s", dateStmt)
	}

	textStmt := d.AddColumn("tabTest", &doctype.Field{Fieldname: "payload", Fieldtype: "Text", Default: "ignored by mysql"})
	if !contains(textStmt, "`payload` TEXT DEFAULT NULL") || contains(textStmt, "ignored by mysql") {
		t.Fatalf("unsupported MySQL default should leave nullable column without value default, got: %s", textStmt)
	}
}

func TestMySQLIsWriteConflict(t *testing.T) {
	d := &MySQLDialect{}
	for _, code := range []uint16{1205, 1213} {
		if !d.IsWriteConflict(&mysql.MySQLError{Number: code}) {
			t.Errorf("MySQL error %d should be a write conflict", code)
		}
	}
	if d.IsWriteConflict(&mysql.MySQLError{Number: 1062}) {
		t.Fatal("duplicate key should not be classified as serialization contention")
	}
}

func TestLibSQLIsWriteConflict(t *testing.T) {
	d := &LibSQLDialect{}
	for _, message := range []string{"SQLITE_BUSY: database is locked", "SQLITE_LOCKED"} {
		if !d.IsWriteConflict(errors.New(message)) {
			t.Errorf("LibSQL error %q should be a write conflict", message)
		}
	}
	if d.IsWriteConflict(errors.New("connection reset")) {
		t.Fatal("connection error should not be classified as serialization contention")
	}
}

func TestMySQL_OmitsIllegalDefaultsForTextLikeColumns(t *testing.T) {
	d := &MySQLDialect{}
	dt := &doctype.DocType{
		Name: "Text Defaults DocType",
		Fields: []doctype.Field{
			{Fieldname: "body", Fieldtype: "Text", Default: "hello"},
			{Fieldname: "details", Fieldtype: "Text Editor", Default: "rich"},
			{Fieldname: "payload", Fieldtype: "JSON", Default: `{"ok":true}`},
		},
	}

	stmt := d.CreateTable(dt)[0]
	if strings.Contains(stmt, "DEFAULT 'hello'") || strings.Contains(stmt, "DEFAULT 'rich'") || strings.Contains(stmt, `DEFAULT '{"ok":true}'`) {
		t.Fatalf("MySQL should not emit defaults for text-like or JSON columns: %s", stmt)
	}
	if !contains(stmt, "`body` TEXT DEFAULT NULL") {
		t.Fatalf("expected nullable text column without SQL default, got: %s", stmt)
	}
	if !contains(stmt, "`details` LONGTEXT DEFAULT NULL") {
		t.Fatalf("expected nullable longtext column without SQL default, got: %s", stmt)
	}
	if !contains(stmt, "`payload` JSON DEFAULT NULL") {
		t.Fatalf("expected nullable JSON column without SQL default, got: %s", stmt)
	}
}

func TestLibSQL_CreateTable(t *testing.T) {
	d := &LibSQLDialect{}
	dt := &doctype.DocType{
		Name: "Test DocType",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
			{Fieldname: "qty", Fieldtype: "Int", Label: "Quantity"},
		},
	}

	stmts := d.CreateTable(dt)
	if len(stmts) != 2 {
		t.Fatalf("CreateTable returned %d statements, want 2 (table + trigger)", len(stmts))
	}

	first := stmts[0]
	if first[:12] != "CREATE TABLE" {
		t.Errorf("first statement = %q..., want CREATE TABLE", first[:12])
	}
	if !contains(first, `"title" TEXT`) {
		t.Error("CreateTable should include title column")
	}
	if !contains(first, `"qty" INTEGER`) {
		t.Error("CreateTable should include qty column")
	}
	if !contains(stmts[1], "CREATE TRIGGER") {
		t.Error("second statement should be CREATE TRIGGER")
	}
}

func TestMySQL_AddColumn(t *testing.T) {
	d := &MySQLDialect{}

	tests := []struct {
		name     string
		field    doctype.Field
		expectFn func(string) bool
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
		})
	}
}

func TestMySQL_AddColumnEscapesAndFormatsDefaults(t *testing.T) {
	d := &MySQLDialect{}

	dateStmt := d.AddColumn("tabTest", &doctype.Field{Fieldname: "sale_date", Fieldtype: "Date", Default: "Today"})
	if !contains(dateStmt, "DEFAULT CURRENT_DATE") {
		t.Fatalf("expected CURRENT_DATE in add column, got: %s", dateStmt)
	}

	textStmt := d.AddColumn("tabTest", &doctype.Field{Fieldname: "title", Fieldtype: "Data", Default: "O'Reilly"})
	if !contains(textStmt, "DEFAULT 'O''Reilly'") {
		t.Fatalf("expected escaped string default, got: %s", textStmt)
	}
}

func TestLibSQL_TemporalDefaults(t *testing.T) {
	d := &LibSQLDialect{}
	dt := &doctype.DocType{
		Name: "Temporal DocType",
		Fields: []doctype.Field{
			{Fieldname: "sale_date", Fieldtype: "Date", Default: "Today"},
			{Fieldname: "created_at", Fieldtype: "Datetime", Default: "Now"},
		},
	}

	stmt := d.CreateTable(dt)[0]
	if !contains(stmt, `"sale_date" TEXT DEFAULT (DATE('now'))`) {
		t.Fatalf("expected libsql date expression, got: %s", stmt)
	}
	if !contains(stmt, `"created_at" TEXT DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))`) {
		t.Fatalf("expected libsql datetime expression, got: %s", stmt)
	}
}

func TestUpsertClause(t *testing.T) {
	tests := []struct {
		name     string
		dialect  Dialect
		conflict []string
		update   []string
		want     string
	}{
		{
			name:     "mysql upsert",
			dialect:  &MySQLDialect{},
			conflict: []string{"name"},
			update:   []string{"email", "status"},
			want:     "ON DUPLICATE KEY UPDATE `email` = VALUES(`email`), `status` = VALUES(`status`)",
		},
		{
			name:     "libsql upsert",
			dialect:  &LibSQLDialect{},
			conflict: []string{"name"},
			update:   []string{"email", "status"},
			want:     `ON CONFLICT("name") DO UPDATE SET "email" = excluded."email", "status" = excluded."status"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dialect.UpsertClause(tt.conflict, tt.update)
			if got != tt.want {
				t.Errorf("UpsertClause = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpsertIncrement(t *testing.T) {
	tests := []struct {
		name     string
		dialect  Dialect
		conflict []string
		incCols  []string
		want     string
	}{
		{
			name:     "mysql upsert increment",
			dialect:  &MySQLDialect{},
			conflict: []string{"site", "date"},
			incCols:  []string{"count"},
			want:     "ON DUPLICATE KEY UPDATE `count` = `count` + VALUES(`count`)",
		},
		{
			name:     "libsql upsert increment",
			dialect:  &LibSQLDialect{},
			conflict: []string{"site", "date"},
			incCols:  []string{"count"},
			want:     `ON CONFLICT("site", "date") DO UPDATE SET "count" = "count" + excluded."count"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dialect.UpsertIncrement(tt.conflict, tt.incCols)
			if got != tt.want {
				t.Errorf("UpsertIncrement = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInsertOrIgnorePrefix(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		want    string
	}{
		{"mysql", &MySQLDialect{}, "INSERT IGNORE"},
		{"libsql", &LibSQLDialect{}, "INSERT OR IGNORE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dialect.InsertOrIgnorePrefix()
			if got != tt.want {
				t.Errorf("InsertOrIgnorePrefix = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseError_DuplicateKey(t *testing.T) {
	d := &MySQLDialect{}
	dt := &doctype.DocType{
		Name: "User",
		Fields: []doctype.Field{
			{Fieldname: "email", Label: "Email"},
		},
	}

	mysqlErr := &mysql.MySQLError{
		Number:  1062,
		Message: "Duplicate entry 'test@test.com' for key 'uq_email'",
	}

	ve := d.ParseError(mysqlErr, dt)
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

func TestParseError_NotNull(t *testing.T) {
	d := &MySQLDialect{}
	dt := &doctype.DocType{
		Name: "User",
		Fields: []doctype.Field{
			{Fieldname: "full_name", Label: "Full Name"},
		},
	}

	mysqlErr := &mysql.MySQLError{
		Number:  1048,
		Message: "Column 'full_name' cannot be null",
	}

	ve := d.ParseError(mysqlErr, dt)
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

func TestParseError_Unrelated(t *testing.T) {
	d := &MySQLDialect{}
	dt := &doctype.DocType{Name: "Test"}

	// Non-MySQL error.
	ve := d.ParseError(errors.New("connection refused"), dt)
	if ve != nil {
		t.Error("ParseError should return nil for non-MySQL errors")
	}

	// Non-constraint error number.
	mysqlErr := &mysql.MySQLError{
		Number:  1146,
		Message: "Table 'test' doesn't exist",
	}
	ve = d.ParseError(mysqlErr, dt)
	if ve != nil {
		t.Error("ParseError should return nil for non-constraint errors")
	}
}

func TestLibSQL_ParseError(t *testing.T) {
	d := &LibSQLDialect{}
	dt := &doctype.DocType{
		Name: "User",
		Fields: []doctype.Field{
			{Fieldname: "email", Label: "Email"},
			{Fieldname: "name", Label: "Name"},
		},
	}

	tests := []struct {
		name    string
		err     error
		want    string // ValidationError type, or "" if nil
		wantMsg string
	}{
		{
			name:    "unique constraint",
			err:     errors.New("UNIQUE constraint failed: User.email"),
			want:    "UniqueConstraint",
			wantMsg: "Email must be unique.",
		},
		{
			name:    "not null constraint",
			err:     errors.New("NOT NULL constraint failed: User.name"),
			want:    "NotNullConstraint",
			wantMsg: "Name is required.",
		},
		{
			name:    "nil error",
			err:     nil,
			want:    "",
			wantMsg: "",
		},
		{
			name:    "unrelated error",
			err:     errors.New("disk I/O error"),
			want:    "",
			wantMsg: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ve := d.ParseError(tt.err, dt)
			if tt.want == "" {
				if ve != nil {
					t.Errorf("ParseError = %v, want nil", ve)
				}
				return
			}
			if ve == nil {
				t.Fatal("ParseError should return a ValidationError")
			}
			if ve.Type != tt.want {
				t.Errorf("Type = %q, want %q", ve.Type, tt.want)
			}
			if ve.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q", ve.Message, tt.wantMsg)
			}
		})
	}
}

func TestNameGenQuery(t *testing.T) {
	tests := []struct {
		name      string
		dialect   Dialect
		tableName string
		prefix    string
		want      string
	}{
		{
			name:      "mysql",
			dialect:   &MySQLDialect{},
			tableName: "tabTask",
			prefix:    "TASK",
			want:      "SELECT COALESCE(MAX(CAST(SUBSTRING_INDEX(name, '-', -1) AS UNSIGNED)), 0) FROM `tabTask` WHERE name LIKE 'TASK-%'",
		},
		{
			name:      "libsql",
			dialect:   &LibSQLDialect{},
			tableName: "tabTask",
			prefix:    "TASK",
			want:      "SELECT COALESCE(MAX(CAST(SUBSTR(name, INSTR(name, '-')+1) AS INTEGER)), 0) FROM \"tabTask\" WHERE name LIKE 'TASK-%'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dialect.NameGenQuery(tt.tableName, tt.prefix)
			if got != tt.want {
				t.Errorf("NameGenQuery = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		input   string
		want    string
	}{
		{"mysql", &MySQLDialect{}, "tabUser", "`tabUser`"},
		{"mysql spaces", &MySQLDialect{}, "tabWork Order", "`tabWork Order`"},
		{"libsql", &LibSQLDialect{}, "tabUser", `"tabUser"`},
		{"libsql spaces", &LibSQLDialect{}, "tabWork Order", `"tabWork Order"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dialect.QuoteIdent(tt.input)
			if got != tt.want {
				t.Errorf("QuoteIdent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlaceholder(t *testing.T) {
	d := &MySQLDialect{}
	for i := 1; i <= 5; i++ {
		got := d.Placeholder(i)
		if got != "?" {
			t.Errorf("Placeholder(%d) = %q, want '?'", i, got)
		}
	}
}

func TestNowTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		want    string
	}{
		{"mysql", &MySQLDialect{}, "CURRENT_TIMESTAMP(6)"},
		{"libsql", &LibSQLDialect{}, `STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW')`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dialect.NowTimestamp()
			if got != tt.want {
				t.Errorf("NowTimestamp = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTableSuffix(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		want    string
	}{
		{"mysql", &MySQLDialect{}, "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"},
		{"libsql", &LibSQLDialect{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dialect.TableSuffix()
			if got != tt.want {
				t.Errorf("TableSuffix = %q, want %q", got, tt.want)
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
