//go:build integration

package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// A before_save script sees the derived child and parent values of the document it
// is saving: each line_total, then the total built on them. The script copies both
// into Data fields, and the test compares them with the stored values on create and
// update. A script that changes a child quantity has its change derived again, so the
// stored line_total and total follow it. Chained parent fields (a total built on a
// subtotal, a figure built on the total) must read their inputs as derived, whatever
// order they are declared in, and the real Sale must show the same.
func TestPackScriptsSeeDerivedTotals(t *testing.T) {
	pack := writeDeriveTotalsPack(t)
	eachPackDialect(t, []string{pack}, func(f *packFixture) {
		created := f.create("Derive Order", `{"items":[{"quantity":2,"price":5},{"quantity":1,"price":7}]}`)
		assertSeenTotals(f, created, 17, 10)
		assertStoredTotals(f, created, 17, []float64{10, 7})

		f.update("Derive Order", created, `{"items":[{"quantity":4,"price":5},{"quantity":1,"price":7}]}`)
		assertSeenTotals(f, created, 27, 20)
		assertStoredTotals(f, created, 27, []float64{20, 7})

		bumped := f.create("Derive Order", `{"note":"bump","items":[{"quantity":2,"price":5}]}`)
		assertStoredTotals(f, bumped, 15, []float64{15})
	})

	chain := writeChainTotalsPack(t)
	eachPackDialect(t, []string{chain}, func(f *packFixture) {
		for _, parent := range []string{"Chain Before", "Chain After"} {
			f.run(parent, func(f *packFixture) {
				created := f.create(parent, `{"items":[{"quantity":2,"price":5},{"quantity":1,"price":7}]}`)
				assertChainTotals(f, parent, created, 17, 34)
				f.update(parent, created, `{"items":[{"quantity":4,"price":5},{"quantity":1,"price":7}]}`)
				assertChainTotals(f, parent, created, 27, 54)
			})
		}
	})

	sale := copyErpSellingPack(t)
	writeTestFile(t, filepath.Join(sale, "scripts", "Sale", "before_save.js"), `var doc = __kora_event__.doc;
doc.notes = "seen|" + doc.total + "|" + doc.base_total;
return {doc: doc};
`)
	eachPackDialect(t, []string{sale}, func(f *packFixture) {
		f.exec("INSERT INTO "+f.quote("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 2, ?)", "USD", "USD", "USD", true)
		product := f.create("Product", `{"product_name":"Mandazi","selling_price":5}`)
		body := func(quantity string) string {
			return `{"sale_date":"2026-11-20","status":"Draft","currency":"USD","items":[{"product":"` + product + `","quantity":` + quantity + `,"unit_price":5}]}`
		}
		created := f.create("Sale", body("2"))
		assertSaleSeen(f, created, 10, 20)
		assertSaleStored(f, created, 10, 20)

		f.update("Sale", created, body("3"))
		assertSaleSeen(f, created, 15, 30)
		assertSaleStored(f, created, 15, 30)
	})

	// A paid sale saves each product it draws on with the new stock. The product's
	// before_save script must see the computed stock_view built on that stock.
	effect := copyErpSellingPack(t)
	appendTestFile(t, filepath.Join(effect, "doctypes", "product.yaml"), `  - fieldname: stock_view
    fieldtype: Float
    label: Stock view
    computed: "stock_qty * 2"
    read_only: true
  - fieldname: seen_note
    fieldtype: Data
    label: Seen note
`)
	writeTestFile(t, filepath.Join(effect, "scripts", "Product", "before_save.js"), `var doc = __kora_event__.doc;
doc.seen_note = "seen|" + doc.stock_qty + "|" + doc.stock_view;
return {doc: doc};
`)
	eachPackDialect(t, []string{effect}, func(f *packFixture) {
		product := f.create("Product", `{"product_name":"Mandazi","selling_price":5,"stock_qty":10}`)
		f.create("Sale", `{"sale_date":"2026-11-20","status":"Paid","items":[{"product":"`+product+`","quantity":2,"unit_price":5}]}`)
		if got := f.amount("Product", product, "stock_qty"); got != 8 {
			f.t.Fatalf("stored stock_qty = %v, want 8", got)
		}
		if got := f.amount("Product", product, "stock_view"); got != 16 {
			f.t.Fatalf("stored stock_view = %v, want 16", got)
		}
		parts := strings.Split(f.value("Product", product, "seen_note"), "|")
		if len(parts) != 3 || parts[0] != "seen" || parts[1] != "8" || parts[2] != "16" {
			f.t.Fatalf("product script saw %q, want seen|8|16", strings.Join(parts, "|"))
		}
	})
}

// assertChainTotals checks the stored subtotal chain of a chain parent and the values the
// script saw for total and grand (grand = total * 2).
func assertChainTotals(f *packFixture, parent, name string, wantTotal, wantGrand float64) {
	f.t.Helper()
	if got := f.amount(parent, name, "total"); got != wantTotal {
		f.t.Fatalf("stored total = %v, want %v", got, wantTotal)
	}
	if got := f.amount(parent, name, "grand"); got != wantGrand {
		f.t.Fatalf("stored grand = %v, want %v", got, wantGrand)
	}
	if got := floatColumn(f, parent, name, "seen_total"); got != wantTotal {
		f.t.Fatalf("script saw total %v, want %v", got, wantTotal)
	}
	if got := floatColumn(f, parent, name, "seen_grand"); got != wantGrand {
		f.t.Fatalf("script saw grand %v, want %v", got, wantGrand)
	}
}

// assertSaleSeen checks the total and base total the Sale's before_save script read.
// The script writes "seen|<total>|<base_total>" into notes.
func assertSaleSeen(f *packFixture, name string, wantTotal, wantBase float64) {
	f.t.Helper()
	parts := strings.Split(f.value("Sale", name, "notes"), "|")
	if len(parts) != 3 || parts[0] != "seen" {
		f.t.Fatalf("sale notes = %q, want seen|total|base", strings.Join(parts, "|"))
	}
	total, errTotal := strconv.ParseFloat(parts[1], 64)
	base, errBase := strconv.ParseFloat(parts[2], 64)
	if errTotal != nil || errBase != nil {
		f.t.Fatalf("sale notes %q are not numbers", strings.Join(parts, "|"))
	}
	if total != wantTotal {
		f.t.Fatalf("script saw sale total %v, want %v", total, wantTotal)
	}
	if base != wantBase {
		f.t.Fatalf("script saw sale base_total %v, want %v", base, wantBase)
	}
}

// assertSaleStored checks the stored total and base total of a Sale.
func assertSaleStored(f *packFixture, name string, wantTotal, wantBase float64) {
	f.t.Helper()
	if got := f.amount("Sale", name, "total"); got != wantTotal {
		f.t.Fatalf("stored sale total = %v, want %v", got, wantTotal)
	}
	if got := f.amount("Sale", name, "base_total"); got != wantBase {
		f.t.Fatalf("stored sale base_total = %v, want %v", got, wantBase)
	}
}

// copyErpSellingPack copies the shipped selling pack into a temporary directory so a
// test can add files or edit doctypes. The shipped pack is not modified.
func copyErpSellingPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := filepath.WalkDir(erpSellingPack, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(erpSellingPack, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
	if err != nil {
		t.Fatalf("copy selling pack: %v", err)
	}
	return root
}

// appendTestFile appends text to an existing file of a test pack.
func appendTestFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(text); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeChainTotalsPack writes two parents with the same child table. Each total chain is
// subtotal = SUM(items.line_total), total = subtotal, grand = total * 2. Chain Before
// declares grand and total before subtotal; Chain After declares them in dependency order.
// Both scripts copy the total and grand they see into Data fields.
func writeChainTotalsPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	chainFields := func(order []string) string {
		fields := map[string]string{
			"grand":    "  - fieldname: grand\n    fieldtype: Currency\n    label: Grand\n    computed: \"total * 2\"\n    read_only: true\n",
			"total":    "  - fieldname: total\n    fieldtype: Currency\n    label: Total\n    computed: \"subtotal\"\n    read_only: true\n",
			"subtotal": "  - fieldname: subtotal\n    fieldtype: Currency\n    label: Subtotal\n    computed: \"SUM(items.line_total)\"\n    read_only: true\n",
		}
		var out strings.Builder
		for _, name := range order {
			out.WriteString(fields[name])
		}
		return out.String()
	}
	parent := func(name string, order []string) string {
		return "name: " + name + "\nmodule: Chain\nfields:\n" +
			"  - fieldname: items\n    fieldtype: Table\n    label: Items\n    options: Chain Item\n" +
			chainFields(order) +
			"  - fieldname: seen_total\n    fieldtype: Data\n    label: Seen total\n" +
			"  - fieldname: seen_grand\n    fieldtype: Data\n    label: Seen grand\n"
	}
	writeTestFile(t, filepath.Join(root, "doctypes", "chain-before.yaml"), parent("Chain Before", []string{"grand", "total", "subtotal"}))
	writeTestFile(t, filepath.Join(root, "doctypes", "chain-after.yaml"), parent("Chain After", []string{"subtotal", "total", "grand"}))
	writeTestFile(t, filepath.Join(root, "doctypes", "chain-item.yaml"), `name: Chain Item
module: Chain
is_child_table: true
fields:
  - fieldname: quantity
    fieldtype: Float
    label: Quantity
  - fieldname: price
    fieldtype: Currency
    label: Price
  - fieldname: line_total
    fieldtype: Currency
    label: Line total
    computed: "quantity * price"
    read_only: true
`)
	script := `var doc = __kora_event__.doc;
doc.seen_total = String(doc.total);
doc.seen_grand = String(doc.grand);
return {doc: doc};
`
	writeTestFile(t, filepath.Join(root, "scripts", "Chain Before", "before_save.js"), script)
	writeTestFile(t, filepath.Join(root, "scripts", "Chain After", "before_save.js"), script)
	return root
}

// writeTestFile writes body to path, creating its directory.
func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertSeenTotals checks the total and first line_total the script read on save.
func assertSeenTotals(f *packFixture, name string, wantTotal, wantLine float64) {
	f.t.Helper()
	if got := floatColumn(f, "Derive Order", name, "seen_total"); got != wantTotal {
		f.t.Fatalf("script saw total %v, want %v", got, wantTotal)
	}
	if got := floatColumn(f, "Derive Order", name, "seen_line"); got != wantLine {
		f.t.Fatalf("script saw first line_total %v, want %v", got, wantLine)
	}
}

// assertStoredTotals checks the stored parent total and each child line_total in order.
func assertStoredTotals(f *packFixture, parent string, wantTotal float64, wantLines []float64) {
	f.t.Helper()
	if got := f.amount("Derive Order", parent, "total"); got != wantTotal {
		f.t.Fatalf("stored total = %v, want %v", got, wantTotal)
	}
	rows, err := f.db.Query(kdb.Rebind(kdb.Resolve(f.dialect), "SELECT line_total FROM "+f.quote("tabDerive Order__items")+" WHERE parent = ? ORDER BY idx"), parent)
	if err != nil {
		f.t.Fatalf("read child rows: %v", err)
	}
	defer rows.Close()
	var got []float64
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			f.t.Fatalf("scan child row: %v", err)
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			f.t.Fatalf("child line_total %q: %v", text, err)
		}
		got = append(got, value)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read child rows: %v", err)
	}
	if len(got) != len(wantLines) {
		f.t.Fatalf("child rows = %v, want %v", got, wantLines)
	}
	for i := range wantLines {
		if got[i] != wantLines[i] {
			f.t.Fatalf("child line_total[%d] = %v, want %v", i, got[i], wantLines[i])
		}
	}
}

// floatColumn reads a Data column that holds a number the script wrote.
func floatColumn(f *packFixture, recordType, name, column string) float64 {
	f.t.Helper()
	value, err := strconv.ParseFloat(f.value(recordType, name, column), 64)
	if err != nil {
		f.t.Fatalf("%s %s.%s is not a number: %v", recordType, name, column, err)
	}
	return value
}

// writeDeriveTotalsPack writes a parent with a child table whose line_total is computed
// from quantity and price, a parent total summed from the lines, and a before_save script.
func writeDeriveTotalsPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "doctypes", "derive-order.yaml"), `name: Derive Order
module: Derive
fields:
  - fieldname: note
    fieldtype: Data
    label: Note
  - fieldname: items
    fieldtype: Table
    label: Items
    options: Derive Item
  - fieldname: total
    fieldtype: Currency
    label: Total
    computed: "SUM(items.line_total)"
    read_only: true
  - fieldname: seen_total
    fieldtype: Data
    label: Seen total
  - fieldname: seen_line
    fieldtype: Data
    label: Seen line
`)
	writeTestFile(t, filepath.Join(root, "doctypes", "derive-item.yaml"), `name: Derive Item
module: Derive
is_child_table: true
fields:
  - fieldname: quantity
    fieldtype: Float
    label: Quantity
  - fieldname: price
    fieldtype: Currency
    label: Price
  - fieldname: line_total
    fieldtype: Currency
    label: Line total
    computed: "(round (* (round quantity 9) (round price 9)) 2)"
    read_only: true
`)
	writeTestFile(t, filepath.Join(root, "scripts", "Derive Order", "before_save.js"), `var doc = __kora_event__.doc;
if (doc.note === "bump") {
  doc.items[0].quantity = 3;
}
doc.seen_total = String(doc.total);
doc.seen_line = String(doc.items[0].line_total);
return {doc: doc};
`)
	return root
}
