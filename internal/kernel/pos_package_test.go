package kernel

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestPOSPackageInstallable(t *testing.T) {
	root := filepath.Join("..", "..", "config", "pos")
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
	item, _ := os.ReadFile(filepath.Join("..", "..", "config", "inventory", "doctypes", "item.yaml"))
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

// TestPOSRecordSaleRefusesPaymentsThatDoNotEqualTotal drives the shipped
// record_sale pack command through its compute block and equality guard.
func TestPOSRecordSaleRefusesPaymentsThatDoNotEqualTotal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "pos", "commands", "record_sale.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := ParseCommandResource(raw)
	if err != nil {
		t.Fatal(err)
	}
	var guard *CommandGuard
	for i := range cmd.Require {
		if len(cmd.Require[i].Equal) > 0 {
			guard = &cmd.Require[i]
		}
	}
	if guard == nil {
		t.Fatal("record_sale has no equality guard on payments")
	}
	line := func(qty, price float64) map[string]any {
		return map[string]any{"item": "ITEM-1", "quantity": qty, "unit_price": price}
	}
	pay := func(amount float64) map[string]any {
		return map[string]any{"method": "Cash", "amount": amount, "reference": "r"}
	}
	cases := []struct {
		name     string
		items    []any
		payments []any
		refused  bool
	}{
		{"empty payments", []any{line(1, 65)}, []any{}, true},
		{"short", []any{line(2, 65)}, []any{pay(65)}, true},
		{"exact", []any{line(2, 65)}, []any{pay(130)}, false},
		{"over (2.02 paid 3.01)", []any{line(1, 2.02)}, []any{pay(3.01)}, true},
		{"zero total, no payments", []any{}, []any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"items": tc.items, "payments": tc.payments}
			computed, err := evalCompute(cmd.Compute, input)
			if err != nil {
				t.Fatal(err)
			}
			cerr := checkEqualGuard(*guard, refEnv{input: input, computed: computed}, 0)
			if (cerr != nil) != tc.refused {
				t.Fatalf("error = %v, refused want %v", cerr, tc.refused)
			}
			if cerr != nil && cerr.Message != "Payments must add up to the sale total" {
				t.Fatalf("message = %q", cerr.Message)
			}
		})
	}
}
