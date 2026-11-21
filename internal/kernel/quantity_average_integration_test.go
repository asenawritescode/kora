//go:build integration

package kernel_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
)

// averageFixture is stock with a moving average cost. Product holds stock_qty and
// cost_price. Goods Receipt (ungated, lines) and Stock Return (gated by status,
// lines) raise stock at a unit_cost each line carries. Sale (ungated, a decrease)
// also names a unit_cost, so the test can show that a decrease leaves the average alone.
func averageFixture(t *testing.T, driver, dsnEnv string) *siteFixture {
	t.Helper()
	average := &doctype.AverageCost{TargetField: "cost_price", CostField: "unit_cost"}
	product := &doctype.DocType{
		Name: "Product", Module: "Kernel Test", NamingSeries: "PROD-.#####",
		Fields: []doctype.Field{{Fieldname: "stock_qty", Fieldtype: "Float", Label: "Stock"}, {Fieldname: "cost_price", Fieldtype: "Float", Label: "Cost"}},
	}
	line := &doctype.DocType{Name: "Receipt Line", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
		{Fieldname: "unit_cost", Fieldtype: "Float"},
	}}
	receipt := &doctype.DocType{
		Name: "Goods Receipt", Module: "Kernel Test", NamingSeries: "GR-.#####",
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items",
			SourceRecordField: "product", DeltaField: "quantity", Direction: "increase", AverageCost: average,
		},
		Fields: []doctype.Field{{Fieldname: "items", Fieldtype: "Table", Options: "Receipt Line"}},
	}
	returnLine := &doctype.DocType{Name: "Return Line", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
		{Fieldname: "unit_cost", Fieldtype: "Float"},
	}}
	stockReturn := &doctype.DocType{
		Name: "Stock Return", Module: "Kernel Test", NamingSeries: "RET-.#####",
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items",
			SourceRecordField: "product", DeltaField: "quantity", Direction: "increase", AverageCost: average,
			StatusField: "status", ApplyInStatuses: []string{"Received"},
		},
		Fields: []doctype.Field{
			{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nReceived", Default: "Draft"},
			{Fieldname: "items", Fieldtype: "Table", Options: "Return Line"},
		},
	}
	sale := &doctype.DocType{
		Name: "Sale", Module: "Kernel Test", NamingSeries: "SALE-.#####",
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product",
			DeltaField: "quantity", Direction: "decrease", AverageCost: average,
		},
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "quantity", Fieldtype: "Float"},
			{Fieldname: "unit_cost", Fieldtype: "Float"},
		},
	}
	perms := []*doctype.Permission{
		{Doctype: "Product", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true},
		{Doctype: "Goods Receipt", Role: "Administrator", Read: true, Create: true},
		{Doctype: "Receipt Line", Role: "Administrator", Read: true, Create: true},
		{Doctype: "Stock Return", Role: "Administrator", Read: true, Write: true, Create: true},
		{Doctype: "Return Line", Role: "Administrator", Read: true, Create: true},
		{Doctype: "Sale", Role: "Administrator", Read: true, Create: true},
	}
	return newDisposableFixture(t, driver, dsnEnv, []*doctype.DocType{product, line, receipt, returnLine, stockReturn, sale}, perms)
}

func forEachAverageDialect(t *testing.T, check func(t *testing.T, s *siteFixture, k *kernel.Kernel)) {
	t.Helper()
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			s := averageFixture(t, tc.driver, tc.env)
			defer s.DB.Close()
			k := newKernel(s)
			k.Commands = kernel.NewCommandRegistry()
			check(t, s, k)
		})
	}
}

// productQtyAndCost reads a product's stock and moving average; cost is nil until a move sets it.
func productQtyAndCost(t *testing.T, s *siteFixture, name string) (float64, sql.NullFloat64) {
	t.Helper()
	table := s.Dialect.QuoteIdent(s.Registry.Get("Product").RawTableName())
	var qty float64
	var cost sql.NullFloat64
	if err := s.DB.QueryRow("SELECT stock_qty, cost_price FROM "+table+" WHERE name = ?", name).Scan(&qty, &cost); err != nil {
		t.Fatalf("read product %s: %v", name, err)
	}
	return qty, cost
}

func expectAverage(t *testing.T, s *siteFixture, product string, wantQty, wantCost float64) {
	t.Helper()
	qty, cost := productQtyAndCost(t, s, product)
	if qty != wantQty || !cost.Valid || cost.Float64 != wantCost {
		t.Fatalf("%s stock = %v, cost = %+v; want stock %v, cost %v", product, qty, cost, wantQty, wantCost)
	}
}

// An ungated receipt with lines moves the average on the same write that moves stock:
// nothing on hand takes the cost, then 10 at 70 onto 10 at 50 averages 60.
func TestReceiptLinesUpdateTheMovingAverageOnBothDialects(t *testing.T) {
	forEachAverageDialect(t, func(t *testing.T, s *siteFixture, k *kernel.Kernel) {
		p := seedLedgerProducts(t, s, k, 0)[0]
		receipt := func(cost float64, qty int) {
			data := map[string]any{"items": []map[string]any{{"product": p, "quantity": qty, "unit_cost": cost}}}
			payload, _ := json.Marshal(map[string]any{"doctype": "Goods Receipt", "data": data})
			mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
		}
		receipt(50, 10)
		expectAverage(t, s, p, 10, 50)
		receipt(70, 10)
		expectAverage(t, s, p, 20, 60)
	})
}

// A gated return moves the average when its status enters the applying set, and
// keeps it unchanged while the record is still a draft. A sale (decrease) leaves it alone.
func TestGatedReturnAndSaleLeaveTheAverageRightOnBothDialects(t *testing.T) {
	forEachAverageDialect(t, func(t *testing.T, s *siteFixture, k *kernel.Kernel) {
		p := seedLedgerProducts(t, s, k, 0)[0]
		payload, _ := json.Marshal(map[string]any{"doctype": "Goods Receipt", "data": map[string]any{"items": []map[string]any{{"product": p, "quantity": 10, "unit_cost": 50}, {"product": p, "quantity": 10, "unit_cost": 70}}}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
		expectAverage(t, s, p, 20, 60)

		sale, _ := json.Marshal(map[string]any{"doctype": "Sale", "data": map[string]any{"product": p, "quantity": 5, "unit_cost": 999}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: sale, Context: opCtx(s.Name, "Administrator")}))
		expectAverage(t, s, p, 15, 60)

		ret, _ := json.Marshal(map[string]any{"doctype": "Stock Return", "data": map[string]any{"items": []map[string]any{{"product": p, "quantity": 5, "unit_cost": 100}}}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: ret, Context: opCtx(s.Name, "Administrator")}))
		expectAverage(t, s, p, 15, 60)

		name := latestName(t, s, "Stock Return")
		update, _ := json.Marshal(map[string]any{"doctype": "Stock Return", "name": name, "data": map[string]any{"status": "Received"}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.update", Payload: update, Context: opCtx(s.Name, "Administrator")}))
		// (15 at 60 + 5 at 100) / 20 = 70
		expectAverage(t, s, p, 20, 70)
	})
}
