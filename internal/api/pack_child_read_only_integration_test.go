//go:build integration

package api

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// A read-only field on a child row is not written by a person's save: the stored value of the
// row with the same name stays, and a new row gets the default or empty. A document-event
// script that saves the parent (trusted) still sets it.
func TestPackScriptsChildReadOnly(t *testing.T) {
	pack := writeChildReadOnlyPack(t)
	eachPackDialect(t, []string{pack}, func(admin *packFixture) {
		clerk := admin.as("Clerk")
		tally := admin.create("Tally", `{"label":"shelf"}`)
		admin.update("Tally", tally, `{"items":[{"label":"first"}]}`)

		admin.run("new child row takes the default for a read-only field", func(f *packFixture) {
			rows := childReadOnlyRows(f, tally)
			if len(rows) != 1 || rows[0].status != "open" || rows[0].counted.Valid {
				f.t.Fatalf("rows = %+v, want one row with status open and empty counted", rows)
			}
		})

		clerk.run("a script's save sets the read-only count on every row", func(f *packFixture) {
			f.create("Tally Source", `{"target":"`+tally+`","note":"stamp"}`)
			rows := childReadOnlyRows(f, tally)
			if len(rows) != 1 || !rows[0].counted.Valid || rows[0].counted.Float64 != 7 {
				f.t.Fatalf("rows = %+v, want counted 7 after the script", rows)
			}
		})

		clerk.run("a person's save keeps the stored read-only values", func(f *packFixture) {
			before := childReadOnlyRows(f, tally)[0]
			body := `{"items":[{"name":"` + before.name + `","label":"renamed","counted":99,"status":"forged"}]}`
			f.update("Tally", tally, body)
			after := childReadOnlyRows(f, tally)[0]
			if after.label != "renamed" {
				f.t.Fatalf("label = %q, want the person's edit", after.label)
			}
			if !after.counted.Valid || after.counted.Float64 != 7 || after.status != before.status {
				f.t.Fatalf("read-only values changed: counted %v status %q, want 7 and %q", after.counted, after.status, before.status)
			}
		})

		clerk.run("a person's new row ignores a read-only count sent with it", func(f *packFixture) {
			f.update("Tally", tally, `{"items":[{"label":"added","counted":5}]}`)
			for _, row := range childReadOnlyRows(f, tally) {
				if row.label == "added" && (row.counted.Valid || row.status != "open") {
					f.t.Fatalf("new row = %+v, want empty counted and status open", row)
				}
			}
		})
	})
}

// childReadOnlyRow is one stored Tally item, read straight from its table.
type childReadOnlyRow struct {
	name, label, status string
	counted             sql.NullFloat64
}

func childReadOnlyRows(f *packFixture, tally string) []childReadOnlyRow {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT name, label, counted, status FROM " + dl.QuoteIdent("tabTally__items") + " WHERE parent = ? ORDER BY idx"
	rows, err := f.db.Query(kdb.Rebind(dl, query), tally)
	if err != nil {
		f.t.Fatalf("read tally items: %v", err)
	}
	defer rows.Close()
	var out []childReadOnlyRow
	for rows.Next() {
		var name, label, status sql.NullString
		var counted sql.NullFloat64
		if err := rows.Scan(&name, &label, &counted, &status); err != nil {
			f.t.Fatalf("scan tally item: %v", err)
		}
		out = append(out, childReadOnlyRow{name: name.String, label: label.String, counted: counted, status: status.String})
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate tally items: %v", err)
	}
	return out
}

func writeChildReadOnlyPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/tally.yaml": `name: Tally
module: Stock
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
  - fieldname: items
    fieldtype: Table
    label: Items
    options: Tally Item
`,
		"doctypes/tally-item.yaml": `name: Tally Item
module: Stock
is_child_table: true
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
  - fieldname: counted
    fieldtype: Float
    label: Counted
    read_only: true
  - fieldname: status
    fieldtype: Data
    label: Status
    default: "open"
    read_only: true
`,
		"doctypes/tally-source.yaml": `name: Tally Source
module: Stock
fields:
  - fieldname: target
    fieldtype: Link
    label: Target
    options: Tally
  - fieldname: note
    fieldtype: Data
    label: Note
`,
		"roles.yaml": `- name: Clerk
  workspace_access: true
`,
		"permissions.yaml": `- doctype: Tally
  role: Clerk
  read: true
  write: true
  create: true
  delete: false
- doctype: Tally Item
  role: Clerk
  read: true
  write: true
  create: true
  delete: false
- doctype: Tally Source
  role: Clerk
  read: true
  write: true
  create: true
  delete: false
`,
		"scripts/Tally Source/before_save.js": `var doc = __kora_event__.doc;
if (doc.note === "stamp") {
  var current = kora.getDoc("Tally", doc.target);
  var rows = [];
  for (var i = 0; i < current.items.length; i++) {
    rows.push({name: current.items[i].name, label: current.items[i].label, counted: 7});
  }
  kora.saveDoc("Tally", {name: doc.target, items: rows});
}
return {doc: doc};
`,
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
