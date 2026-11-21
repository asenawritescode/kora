//go:build integration

package kernel_test

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
)

// recordSaleYAML is a pack command that writes a Sale with the status it is given.
// The Sale's gated quantity effect (see saleStockFixture) decides whether stock moves.
const recordSaleYAML = `
name: record_sale
namespace: pos
version: 1
input:
  record: Sale
  extra:
    sale_status: {type: string, required: true}
    item_product: {type: string, required: true}
    item_quantity: {type: number, required: true}
transaction:
  - create:
      record: Sale
      values: {status: $input.sale_status, product: $input.item_product, quantity: $input.item_quantity}
`

// setSaleStatusYAML is a pack command that moves an existing Sale to a new status.
const setSaleStatusYAML = `
name: set_sale_status
namespace: pos
version: 1
input:
  record: Sale
  extra:
    sale: {type: string, required: true}
    sale_status: {type: string, required: true}
transaction:
  - update:
      record: Sale
      name: $input.sale
      values: {status: $input.sale_status}
`

// saleStockFixture declares Product (stock_qty) and a Sale whose status-gated
// effect takes its quantity from stock while the sale is Awaiting Payment,
// Partially Paid or Paid.
func saleStockFixture(t *testing.T, driver, dsnEnv string) *siteFixture {
	t.Helper()
	product := &doctype.DocType{
		Name: "Product", Module: "Kernel Test", NamingSeries: "PROD-.#####",
		Fields: []doctype.Field{{Fieldname: "stock_qty", Fieldtype: "Float", Label: "Stock"}},
	}
	sale := &doctype.DocType{
		Name: "Sale", Module: "Kernel Test", NamingSeries: "SALE-.YYYY.-.#####",
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product",
			DeltaField: "quantity", Direction: "decrease", StatusField: "status",
			ApplyInStatuses: []string{"Awaiting Payment", "Partially Paid", "Paid"},
		},
		Fields: []doctype.Field{
			{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nAwaiting Payment\nPartially Paid\nPaid\nVoid", Default: "Draft"},
			{Fieldname: "product", Fieldtype: "Link", Options: "Product", Label: "Product"},
			{Fieldname: "quantity", Fieldtype: "Float", Label: "Quantity"},
		},
	}
	perms := []*doctype.Permission{
		{Doctype: "Product", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true},
		{Doctype: "Sale", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true},
	}
	return newDisposableFixture(t, driver, dsnEnv, []*doctype.DocType{product, sale}, perms)
}

// TestCommandWritesMoveStockByStatusOnBothDialects runs pack commands that create
// and update a gated Sale, and checks that stock moves once per status change:
// a Paid create takes stock, a Draft create does not, voiding a Paid sale gives
// it back, and a keyed retry of a Paid create does not take it twice.
func TestCommandWritesMoveStockByStatusOnBothDialects(t *testing.T) {
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			s := saleStockFixture(t, tc.driver, tc.env)
			defer s.DB.Close()
			k := newKernel(s)
			k.Commands = kernel.NewCommandRegistry()
			mustRegister(t, k, recordSaleYAML)
			mustRegister(t, k, setSaleStatusYAML)

			productTable := s.Dialect.QuoteIdent(s.Registry.Get("Product").RawTableName())
			var product string
			seed, _ := json.Marshal(map[string]any{"doctype": "Product", "data": map[string]any{"stock_qty": 10}})
			mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: seed, Context: opCtx(s.Name, "Administrator")}))
			if err := s.DB.QueryRow(db.Rebind(s.Dialect, "SELECT name FROM "+productTable)).Scan(&product); err != nil {
				t.Fatalf("read product: %v", err)
			}
			stock := func() float64 {
				var v float64
				if err := s.DB.QueryRow(db.Rebind(s.Dialect, "SELECT stock_qty FROM "+productTable+" WHERE name = ?"), product).Scan(&v); err != nil {
					t.Fatalf("read stock: %v", err)
				}
				return v
			}
			expect := func(want float64) {
				t.Helper()
				if got := stock(); got != want {
					t.Fatalf("stock_qty = %v, want %v", got, want)
				}
			}
			recordSale := func(status string, key string) contract.CommandResult {
				payload, _ := json.Marshal(map[string]any{"data": map[string]any{"sale_status": status, "item_product": product, "item_quantity": 3}})
				op := kernel.Operation{Command: "pos.record_sale", Payload: payload, Context: opCtx(s.Name, "Administrator")}
				op.Context.IdempotencyKey = key
				return exec(t, s, k, op)
			}
			saleName := func(res contract.CommandResult) string {
				t.Helper()
				var out struct {
					Steps []struct {
						Name string `json:"name"`
					} `json:"steps"`
				}
				if err := json.Unmarshal(res.Data, &out); err != nil || len(out.Steps) != 1 {
					t.Fatalf("read sale name from %s: %v", res.Data, err)
				}
				return out.Steps[0].Name
			}

			paid := recordSale("Paid", "")
			mustComplete(t, paid)
			expect(7)

			mustComplete(t, recordSale("Draft", ""))
			expect(7)

			setStatus := func(sale, status string) contract.CommandResult {
				payload, _ := json.Marshal(map[string]any{"data": map[string]any{"sale": sale, "sale_status": status}})
				return exec(t, s, k, kernel.Operation{Command: "pos.set_sale_status", Payload: payload, Context: opCtx(s.Name, "Administrator")})
			}
			mustComplete(t, setStatus(saleName(paid), "Void"))
			expect(10)

			mustComplete(t, recordSale("Paid", "pos-sale-retry-1"))
			expect(7)
			retried := recordSale("Paid", "pos-sale-retry-1")
			mustComplete(t, retried)
			if !retried.Replayed {
				t.Fatalf("keyed retry was not a replay: %+v", retried)
			}
			expect(7)
		})
	}
}
