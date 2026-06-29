package doctype_test

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func TestMySQL_AddFieldDDL(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "add-field", Section: "doctypes", Entity: "Product", Field: "title",
		Attrs: map[string]any{":type": "Data", ":required": true, ":default": "untitled"},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "ALTER TABLE") {
		t.Errorf("expected ALTER TABLE, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "tabProduct") {
		t.Errorf("expected tabProduct table, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "title") {
		t.Errorf("expected title column, got: %s", stmts[0])
	}
}

func TestMySQL_AddFieldRequiredNoDefault(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "add-field", Section: "doctypes", Entity: "Product", Field: "title",
		Attrs: map[string]any{":type": "Data", ":required": true},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "NOT NULL") {
		t.Errorf("expected NOT NULL, got: %s", stmts[0])
	}
}

func TestMySQL_AddFieldWithUniqueAndIndex(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "add-field", Section: "doctypes", Entity: "Product", Field: "email",
		Attrs: map[string]any{":type": "Data", ":unique": true, ":search_index": true},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) < 2 {
		t.Fatalf("expected at least 2 statements (ADD COLUMN + indexes), got %d", len(stmts))
	}
	foundAlter := false
	foundUnique := false
	for _, s := range stmts {
		if strings.Contains(s, "ALTER TABLE") {
			foundAlter = true
		}
		if strings.Contains(s, "UNIQUE") {
			foundUnique = true
		}
	}
	if !foundAlter {
		t.Error("expected ALTER TABLE statement")
	}
	if !foundUnique {
		t.Error("expected UNIQUE index for unique field")
	}
}

func TestLibSQL_AddFieldDDL(t *testing.T) {
	d := db.Resolve("libsql")
	c := doctype.Change{
		Type: "add-field", Section: "doctypes", Entity: "Product", Field: "title",
		Attrs: map[string]any{":type": "Data", ":required": true, ":default": "untitled"},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "ALTER TABLE") {
		t.Errorf("expected ALTER TABLE, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "tabProduct") {
		t.Errorf("expected tabProduct table, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "title") {
		t.Errorf("expected title column, got: %s", stmts[0])
	}
}

func TestMySQL_CreateTable(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "add-doctype", Section: "doctypes", Entity: "Product",
		Attrs: map[string]any{
			":fields": []any{
				map[string]any{":fieldname": "title", ":type": "Data"},
				map[string]any{":fieldname": "price", ":type": "Currency"},
			},
		},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "CREATE TABLE") {
		t.Errorf("expected CREATE TABLE, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "tabProduct") {
		t.Errorf("expected tabProduct table, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "title") {
		t.Errorf("expected title column, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "price") {
		t.Errorf("expected price column, got: %s", stmts[0])
	}
}

func TestLibSQL_CreateTable(t *testing.T) {
	d := db.Resolve("libsql")
	c := doctype.Change{
		Type: "add-doctype", Section: "doctypes", Entity: "Product",
		Attrs: map[string]any{
			":fields": []any{
				map[string]any{":fieldname": "title", ":type": "Data"},
				map[string]any{":fieldname": "price", ":type": "Currency"},
			},
		},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "CREATE TABLE") {
		t.Errorf("expected CREATE TABLE, got: %s", stmts[0])
	}
}

func TestMySQL_CreateTableWithIndexes(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "add-doctype", Section: "doctypes", Entity: "Product",
		Attrs: map[string]any{
			":fields": []any{
				map[string]any{":fieldname": "title", ":type": "Data", ":search_index": true},
				map[string]any{":fieldname": "sku", ":type": "Data", ":unique": true},
			},
		},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) < 3 {
		t.Fatalf("expected at least 3 statements (CREATE TABLE + 2 indexes), got %d", len(stmts))
	}
}

func TestMySQL_AddIndex(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "add-index", Section: "doctypes", Entity: "Product", Field: "title",
		Attrs: map[string]any{":unique": true},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "CREATE") {
		t.Errorf("expected CREATE, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "UNIQUE") {
		t.Errorf("expected UNIQUE, got: %s", stmts[0])
	}
}

func TestMySQL_RemoveFieldDDL(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "remove-field", Section: "doctypes", Entity: "Product", Field: "obsolete",
		Attrs: map[string]any{":type": "Data"},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "DROP COLUMN") {
		t.Errorf("expected DROP COLUMN, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "quarantine") {
		t.Errorf("expected quarantine comment, got: %s", stmts[0])
	}
}

func TestMySQL_RenameFieldDDL(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "rename-field", Section: "doctypes", Entity: "Product", Field: "new_name",
		Attrs: map[string]any{":renamed-from": "old_name"},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "RENAME COLUMN") {
		t.Errorf("expected RENAME COLUMN, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[0], "old_name") {
		t.Errorf("expected old_name in RENAME, got: %s", stmts[0])
	}
}

func TestMySQL_RenameFieldNoOldName(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "rename-field", Section: "doctypes", Entity: "Product", Field: "new_name",
		Attrs: map[string]any{}, // no :renamed-from
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) != 0 {
		t.Errorf("expected no statements without :renamed-from, got %d", len(stmts))
	}
}

func TestMySQL_AlterColumnDDL(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "change-field-type", Section: "doctypes", Entity: "Product", Field: "price",
		Attrs: map[string]any{":type": "Data", ":old_type": "Currency"},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "MODIFY COLUMN") {
		t.Errorf("expected MODIFY COLUMN, got: %s", stmts[0])
	}
}

func TestMySQL_DropIndex(t *testing.T) {
	d := db.MySQL()
	c := doctype.Change{
		Type: "remove-index", Section: "doctypes", Entity: "Product", Field: "title",
		Attrs: map[string]any{":index_name": "idx_tabProduct_title"},
	}
	stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if !strings.Contains(stmts[0], "DROP INDEX") {
		t.Errorf("expected DROP INDEX, got: %s", stmts[0])
	}
}

func TestDDL_AllChangeTypes(t *testing.T) {
	d := db.MySQL()
	changeTypes := []string{
		"add-doctype", "add-field", "remove-field",
		"rename-field", "change-field-type", "add-index", "remove-index",
	}

	for _, ct := range changeTypes {
		t.Run(ct, func(t *testing.T) {
			c := doctype.Change{
				Type: ct, Section: "doctypes", Entity: "Product", Field: "test_field",
				Attrs: map[string]any{},
			}
			switch ct {
			case "add-doctype":
				c.Attrs[":fields"] = []any{
					map[string]any{":fieldname": "title", ":type": "Data"},
				}
			case "rename-field":
				c.Attrs[":renamed-from"] = "old_name"
			case "change-field-type":
				c.Attrs[":type"] = "Data"
			case "add-index":
				c.Attrs[":unique"] = true
			case "remove-index":
				c.Attrs[":index_name"] = "idx_tabProduct_test_field"
			}
			stmts, err := doctype.GenerateDDLFromDiff([]doctype.Change{c}, d)
			if err != nil {
				t.Fatalf("GenerateDDLFromDiff error for %s: %v", ct, err)
			}
			if len(stmts) == 0 {
				t.Errorf("expected non-empty DDL for change type %s", ct)
			}
		})
	}
}

func TestDDL_NonDoctypeChangesProduceEmpty(t *testing.T) {
	d := db.MySQL()
	changes := []doctype.Change{
		{Type: "add-role", Section: "roles", Entity: "Manager"},
		{Type: "add-perm", Section: "permissions", Entity: "Product|Manager"},
		{Type: "add-workflow", Section: "workflows", Entity: "Product"},
		{Type: "add-metric", Section: "analytics_metrics", Entity: "total_sales"},
		{Type: "add-script-ref", Section: "scripts", Entity: "validate_order"},
		{Type: "remove-role", Section: "roles", Entity: "Manager"},
		{Type: "remove-perm", Section: "permissions", Entity: "Product|Manager"},
		{Type: "remove-workflow", Section: "workflows", Entity: "Product"},
		{Type: "remove-metric", Section: "analytics_metrics", Entity: "total_sales"},
		{Type: "remove-script-ref", Section: "scripts", Entity: "validate_order"},
	}
	stmts, err := doctype.GenerateDDLFromDiff(changes, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) != 0 {
		t.Errorf("expected no DDL for non-doctype changes, got %d statements", len(stmts))
	}
}

func TestDDL_LibSQLGenerate(t *testing.T) {
	d := db.Resolve("libsql")
	changes := []doctype.Change{
		{
			Type: "add-field", Section: "doctypes", Entity: "Product", Field: "title",
			Attrs: map[string]any{":type": "Data", ":required": false},
		},
		{
			Type: "rename-field", Section: "doctypes", Entity: "Product", Field: "new_name",
			Attrs: map[string]any{":renamed-from": "old_name"},
		},
	}
	stmts, err := doctype.GenerateDDLFromDiff(changes, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) != 2 {
		t.Fatalf("expected 2 DDL statements, got %d", len(stmts))
	}
	if !strings.Contains(stmts[0], "ALTER TABLE") {
		t.Errorf("expected ALTER TABLE, got: %s", stmts[0])
	}
	if !strings.Contains(stmts[1], "RENAME COLUMN") {
		t.Errorf("expected RENAME COLUMN, got: %s", stmts[1])
	}
}

func TestDDL_EmptyChanges(t *testing.T) {
	d := db.MySQL()
	stmts, err := doctype.GenerateDDLFromDiff(nil, d)
	if err != nil {
		t.Fatalf("GenerateDDLFromDiff error: %v", err)
	}
	if len(stmts) != 0 {
		t.Errorf("expected no statements for empty changes, got %d", len(stmts))
	}
}
