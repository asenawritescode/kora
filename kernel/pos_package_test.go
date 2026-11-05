package kernel

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestPOSPackageInstallable(t *testing.T) {
	root := filepath.Join("..", "config", "pos")
	pack, _ := os.ReadFile(filepath.Join(root, "pack.yaml"))
	if !strings.Contains(string(pack), "package_id: inventory") {
		t.Fatal("pos pack must require inventory")
	}
	for _, f := range []string{"sale", "sale_item", "sale_payment"} {
		raw, err := os.ReadFile(filepath.Join(root, "doctypes", f+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		dt, err := doctype.ParseYAML(raw, f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		n := 0
		for _, fd := range dt.Fields {
			if fd.Fieldtype == "Currency" {
				n++
				if fd.Options != "currency" {
					t.Fatalf("%s.%s currency = %q", f, fd.Fieldname, fd.Options)
				}
			}
		}
		if c := dt.GetField("currency"); c == nil || c.Default != "KES" {
			t.Fatalf("%s currency default must be KES", f)
		}
		if n == 0 {
			t.Fatalf("%s has no currency field", f)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(root, "commands", "record_sale.yaml"))
	if _, err := ParseCommandResource(raw); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, "demo_catalogue.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) < 6 {
		t.Fatalf("catalogue rows=%d err=%v", len(rows), err)
	}
	item, _ := os.ReadFile(filepath.Join("..", "config", "inventory", "doctypes", "item.yaml"))
	idt, err := doctype.ParseYAML(item, "item")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range rows[0] {
		if idt.GetField(h) == nil {
			t.Fatalf("csv column %q not an Item field", h)
		}
	}
}
