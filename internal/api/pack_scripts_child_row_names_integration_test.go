//go:build integration

package api

import (
	"os"
	"path/filepath"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// A document-event script saves a parent with all its table rows. Rows the script
// sends back keep their names, so the ORM updates them in place; a new row gets a
// name; a row left out is removed. A name that belongs to another parent is refused,
// and a create never reuses row names.
func TestPackScriptsKeepChildRowNames(t *testing.T) {
	pack := writeChildRowNamesPack(t)
	eachPackDialect(t, []string{pack}, func(f *packFixture) {
		f.run("script update keeps row names and changes a field", func(f *packFixture) {
			order := f.create("Script Order", `{"note":"first","lines":[{"label":"a","qty":1},{"label":"b","qty":2}]}`)
			before := f.lineNames(order)
			f.create("Script Trigger", `{"target_order":"`+order+`","action":"bump"}`)
			after := f.lineNames(order)
			if len(after) != 2 || after["a"] != before["a"] || after["b"] != before["b"] {
				f.t.Fatalf("row names after script update = %v, want %v", after, before)
			}
			f.lineValue(before["a"], "qty", "11")
			f.lineValue(before["b"], "qty", "2")
		})

		f.run("script adds a row with a new name and keeps the others", func(f *packFixture) {
			order := f.create("Script Order", `{"note":"add","lines":[{"label":"a","qty":1}]}`)
			before := f.lineNames(order)
			f.create("Script Trigger", `{"target_order":"`+order+`","action":"add"}`)
			after := f.lineNames(order)
			if len(after) != 2 || after["a"] != before["a"] {
				f.t.Fatalf("row names after adding = %v, want %q kept from %v", after, before["a"], before)
			}
			if after["new"] == "" || after["new"] == before["a"] {
				f.t.Fatalf("new row name = %q, want a fresh name", after["new"])
			}
		})

		f.run("script omits a row which is then removed", func(f *packFixture) {
			order := f.create("Script Order", `{"note":"drop","lines":[{"label":"a","qty":1},{"label":"b","qty":2}]}`)
			before := f.lineNames(order)
			f.create("Script Trigger", `{"target_order":"`+order+`","action":"drop"}`)
			after := f.lineNames(order)
			if len(after) != 1 || after["a"] != before["a"] {
				f.t.Fatalf("row names after drop = %v, want only %q kept from %v", after, before["a"], before)
			}
			if got := f.count(f.lineTable(), "name = ?", before["b"]); got != 0 {
				f.t.Fatalf("omitted row %s still stored = %d rows", before["b"], got)
			}
		})

		f.run("saved update keeps row names", func(f *packFixture) {
			order := f.create("Script Order", `{"note":"rest","lines":[{"label":"a","qty":1},{"label":"b","qty":2}]}`)
			before := f.lineNames(order)
			f.update("Script Order", order, `{"lines":[{"name":"`+before["a"]+`","label":"a","qty":5},{"name":"`+before["b"]+`","label":"b","qty":2}]}`)
			after := f.lineNames(order)
			if after["a"] != before["a"] || after["b"] != before["b"] {
				f.t.Fatalf("row names after update = %v, want %v", after, before)
			}
			f.lineValue(before["a"], "qty", "5")
		})

		f.run("row named by another parent is refused and that row is unchanged", func(f *packFixture) {
			mine := f.create("Script Order", `{"note":"mine","lines":[{"label":"a","qty":1}]}`)
			other := f.create("Script Order", `{"note":"other","lines":[{"label":"x","qty":7}]}`)
			foreign := f.lineNames(other)["x"]
			f.refused(httpMethodPut, "Script Order", mine, `{"lines":[{"name":"`+foreign+`","label":"hijack","qty":9}]}`, "is not part of Script Order "+mine)
			f.lineValue(foreign, "parent", other)
			f.lineValue(foreign, "label", "x")
			f.lineValue(foreign, "qty", "7")
		})

		f.run("create with row names gets fresh names", func(f *packFixture) {
			other := f.create("Script Order", `{"note":"source","lines":[{"label":"x","qty":7}]}`)
			foreign := f.lineNames(other)["x"]
			copied := f.create("Script Order", `{"note":"copy","lines":[{"name":"`+foreign+`","label":"z","qty":4}]}`)
			f.lineValue(foreign, "parent", other)
			f.lineValue(foreign, "qty", "7")
			fresh := f.lineNames(copied)["z"]
			if fresh == "" || fresh == foreign {
				f.t.Fatalf("created row name = %q, want a fresh name, not %q", fresh, foreign)
			}
		})
	})
}

// lineTable is the child table holding Script Order's lines.
func (f *packFixture) lineTable() string {
	return "tabScript Order__lines"
}

// lineValue reads one column of one line row, by the row's name, as text.
func (f *packFixture) lineValue(name, column string, want string) {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT " + dl.QuoteIdent(column) + " FROM " + dl.QuoteIdent(f.lineTable()) + " WHERE name = ?"
	var got string
	if err := f.db.QueryRow(kdb.Rebind(dl, query), name).Scan(&got); err != nil {
		f.t.Fatalf("line %s.%s: %v", name, column, err)
	}
	if got != want {
		f.t.Fatalf("line %s.%s = %q, want %q", name, column, got, want)
	}
}

// lineNames returns the name of each line of an order, keyed by its label.
func (f *packFixture) lineNames(order string) map[string]string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT label, name FROM " + dl.QuoteIdent(f.lineTable()) + " WHERE parent = ?"
	rows, err := f.db.Query(kdb.Rebind(dl, query), order)
	if err != nil {
		f.t.Fatalf("list lines of %s: %v", order, err)
	}
	defer rows.Close()
	names := make(map[string]string)
	for rows.Next() {
		var label, name string
		if err := rows.Scan(&label, &name); err != nil {
			f.t.Fatalf("scan line of %s: %v", order, err)
		}
		names[label] = name
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("list lines of %s: %v", order, err)
	}
	return names
}

// writeChildRowNamesPack writes a parent with a table of lines, and a trigger whose
// before_save script edits the parent's lines through kora.saveDoc.
func writeChildRowNamesPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/script-order.yaml": `name: Script Order
module: Rows
fields:
  - fieldname: note
    fieldtype: Data
    label: Note
  - fieldname: lines
    fieldtype: Table
    label: Lines
    options: Script Order Line
`,
		"doctypes/script-order-line.yaml": `name: Script Order Line
module: Rows
is_child_table: true
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
  - fieldname: qty
    fieldtype: Int
    label: Qty
`,
		"doctypes/script-trigger.yaml": `name: Script Trigger
module: Rows
fields:
  - fieldname: target_order
    fieldtype: Link
    label: Order
    options: Script Order
  - fieldname: action
    fieldtype: Data
    label: Action
`,
		"scripts/Script Trigger/before_save.js": `var doc = __kora_event__.doc;
var order = kora.getDoc("Script Order", doc.target_order);
var lines = [];
for (var i = 0; i < order.lines.length; i++) {
  lines.push(order.lines[i]);
}
if (doc.action === "bump") {
  lines[0].qty = lines[0].qty + 10;
  kora.saveDoc("Script Order", {name: doc.target_order, lines: lines});
} else if (doc.action === "add") {
  lines.push({label: "new", qty: 3});
  kora.saveDoc("Script Order", {name: doc.target_order, lines: lines});
} else if (doc.action === "drop") {
  lines.pop();
  kora.saveDoc("Script Order", {name: doc.target_order, lines: lines});
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
