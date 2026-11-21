//go:build integration

package kernel_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
)

// ledgerFixture declares stock with a ledger on every effect: a Goods Receipt
// (ungated, one row per line), a Stock Adjustment (ungated, reason-signed) and a
// Sale (gated by status). Stock Movement is the append-only ledger. Only the
// administrator may touch it directly; the clerk who records receipts may not,
// so every row must come from the source write.
func ledgerFixture(t *testing.T, driver, dsnEnv string) *siteFixture {
	t.Helper()
	ledger := &doctype.QuantityLedger{
		Doctype: "Stock Movement", TargetField: "product", ChangeField: "change", BalanceField: "balance_after",
		SourceDoctypeField: "source_doctype", SourceNameField: "source_name", ReasonField: "reason",
	}
	product := &doctype.DocType{
		Name: "Product", Module: "Kernel Test", NamingSeries: "PROD-.#####",
		Fields: []doctype.Field{{Fieldname: "stock_qty", Fieldtype: "Float", Label: "Stock"}},
	}
	movement := &doctype.DocType{
		Name: "Stock Movement", Module: "Kernel Test", NamingSeries: "MOVE-.#####", AppendOnly: true,
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "change", Fieldtype: "Float"},
			{Fieldname: "balance_after", Fieldtype: "Float"},
			{Fieldname: "source_doctype", Fieldtype: "Data"},
			{Fieldname: "source_name", Fieldtype: "Data"},
			{Fieldname: "reason", Fieldtype: "Data"},
		},
	}
	adjustment := &doctype.DocType{
		Name: "Stock Adjustment", Module: "Kernel Test", NamingSeries: "ADJ-.#####", AppendOnly: true,
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product", DeltaField: "delta",
			ReasonField: "reason", ReasonDirections: map[string]string{"Found": "increase", "Damaged": "decrease"}, Ledger: ledger,
		},
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "delta", Fieldtype: "Float"},
			{Fieldname: "reason", Fieldtype: "Select", Options: "Found\nDamaged"},
		},
	}
	line := &doctype.DocType{Name: "Receipt Line", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
	}}
	receipt := &doctype.DocType{
		Name: "Goods Receipt", Module: "Kernel Test", NamingSeries: "GR-.#####", AppendOnly: true,
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", LinesField: "items",
			SourceRecordField: "product", DeltaField: "quantity", Direction: "increase", Ledger: ledger,
		},
		Fields: []doctype.Field{{Fieldname: "items", Fieldtype: "Table", Options: "Receipt Line"}},
	}
	sale := &doctype.DocType{
		Name: "Sale", Module: "Kernel Test", NamingSeries: "SALE-.YYYY.-.#####",
		QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Product", TargetQuantityField: "stock_qty", SourceRecordField: "product",
			DeltaField: "quantity", Direction: "decrease", StatusField: "status",
			ApplyInStatuses: []string{"Awaiting Payment", "Partially Paid", "Paid"}, Ledger: ledger,
		},
		Fields: []doctype.Field{
			{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nAwaiting Payment\nPartially Paid\nPaid\nVoid", Default: "Draft"},
			{Fieldname: "product", Fieldtype: "Link", Options: "Product", Label: "Product"},
			{Fieldname: "quantity", Fieldtype: "Float", Label: "Quantity"},
		},
	}
	perms := []*doctype.Permission{
		{Doctype: "Product", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true},
		{Doctype: "Product", Role: "Clerk", Read: true},
		{Doctype: "Stock Movement", Role: "Administrator", Read: true, Write: true, Delete: true},
		{Doctype: "Stock Adjustment", Role: "Administrator", Read: true, Create: true},
		{Doctype: "Goods Receipt", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Receipt Line", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Sale", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true},
	}
	return newDisposableFixture(t, driver, dsnEnv, []*doctype.DocType{product, movement, adjustment, line, receipt, sale}, perms)
}

// ledgerRow is one stored Stock Movement row.
type ledgerRow struct {
	Product, SourceDoctype, SourceName, Reason, Owner string
	Change, Balance                                   float64
}

// ledgerRows reads every ledger row in the order they were written.
func ledgerRows(t *testing.T, s *siteFixture) []ledgerRow {
	t.Helper()
	table := s.Dialect.QuoteIdent(s.Registry.Get("Stock Movement").RawTableName())
	query := "SELECT product, " + s.Dialect.QuoteIdent("change") + ", balance_after, source_doctype, source_name, COALESCE(reason, ''), owner FROM " + table + " ORDER BY name"
	rows, err := s.DB.Query(query)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.Product, &r.Change, &r.Balance, &r.SourceDoctype, &r.SourceName, &r.Reason, &r.Owner); err != nil {
			t.Fatalf("scan ledger row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ledger: %v", err)
	}
	return out
}

func expectLedger(t *testing.T, s *siteFixture, want ...ledgerRow) {
	t.Helper()
	if got := ledgerRows(t, s); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger rows\n got: %+v\nwant: %+v", got, want)
	}
}

// latestName returns the name of the most recently named record of a type.
func latestName(t *testing.T, s *siteFixture, doctypeName string) string {
	t.Helper()
	var name string
	table := s.Dialect.QuoteIdent(s.Registry.Get(doctypeName).RawTableName())
	if err := s.DB.QueryRow("SELECT name FROM " + table + " ORDER BY name DESC LIMIT 1").Scan(&name); err != nil {
		t.Fatalf("read %s name: %v", doctypeName, err)
	}
	return name
}

// seedLedgerProducts creates one product per stock figure and returns the
// product names in creation order.
func seedLedgerProducts(t *testing.T, s *siteFixture, k *kernel.Kernel, stock ...float64) []string {
	t.Helper()
	for _, qty := range stock {
		payload, _ := json.Marshal(map[string]any{"doctype": "Product", "data": map[string]any{"stock_qty": qty}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
	}
	table := s.Dialect.QuoteIdent(s.Registry.Get("Product").RawTableName())
	rows, err := s.DB.Query("SELECT name FROM " + table + " ORDER BY name")
	if err != nil {
		t.Fatalf("read products: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func TestQuantityLedgerRecordsEveryMoveOnBothDialects(t *testing.T) {
	forEachLedgerDialect(t, func(t *testing.T, s *siteFixture, k *kernel.Kernel) {
		p := seedLedgerProducts(t, s, k, 10, 10, 10)
		receipt := map[string]any{"items": []map[string]any{
			{"product": p[0], "quantity": 24}, {"product": p[1], "quantity": 12}, {"product": p[0], "quantity": 6}, {"product": p[2], "quantity": 5},
		}}
		payload, _ := json.Marshal(map[string]any{"doctype": "Goods Receipt", "data": receipt})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Clerk")}))
		gr := latestName(t, s, "Goods Receipt")
		// One row per product that moved, with the quantity after that product's change.
		expectLedger(t, s,
			ledgerRow{Product: p[0], Change: 30, Balance: 40, SourceDoctype: "Goods Receipt", SourceName: gr, Owner: "tester"},
			ledgerRow{Product: p[1], Change: 12, Balance: 22, SourceDoctype: "Goods Receipt", SourceName: gr, Owner: "tester"},
			ledgerRow{Product: p[2], Change: 5, Balance: 15, SourceDoctype: "Goods Receipt", SourceName: gr, Owner: "tester"},
		)
	})
}

func TestStockAdjustmentLedgerRowCarriesItsReason(t *testing.T) {
	forEachLedgerDialect(t, func(t *testing.T, s *siteFixture, k *kernel.Kernel) {
		p := seedLedgerProducts(t, s, k, 10)
		for _, adj := range []struct {
			reason string
			delta  int
		}{{"Damaged", 4}, {"Found", 7}} {
			payload, _ := json.Marshal(map[string]any{"doctype": "Stock Adjustment", "data": map[string]any{"product": p[0], "delta": adj.delta, "reason": adj.reason}})
			mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
		}
		rows := ledgerRows(t, s)
		if len(rows) != 2 {
			t.Fatalf("rows = %+v, want two", rows)
		}
		if rows[0].Reason != "Damaged" || rows[0].Change != -4 || rows[0].Balance != 6 || rows[0].SourceDoctype != "Stock Adjustment" {
			t.Fatalf("damaged row = %+v", rows[0])
		}
		if rows[1].Reason != "Found" || rows[1].Change != 7 || rows[1].Balance != 13 || rows[1].SourceDoctype != "Stock Adjustment" {
			t.Fatalf("found row = %+v", rows[1])
		}
	})
}

// A sale writes its moves when its status enters or leaves the applying set:
// Paid takes stock out, Void gives it back, a Draft writes nothing.
func TestGatedSaleLedgerRowsFollowItsStatusOnBothDialects(t *testing.T) {
	forEachLedgerDialect(t, func(t *testing.T, s *siteFixture, k *kernel.Kernel) {
		p := seedLedgerProducts(t, s, k, 10)
		sale := func(status string) {
			payload, _ := json.Marshal(map[string]any{"doctype": "Sale", "data": map[string]any{"status": status, "product": p[0], "quantity": 3}})
			mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
		}
		sale("Draft")
		expectLedger(t, s)
		sale("Paid")
		name := latestName(t, s, "Sale")
		expectLedger(t, s, ledgerRow{Product: p[0], Change: -3, Balance: 7, SourceDoctype: "Sale", SourceName: name, Reason: "Paid", Owner: "tester"})
		update, _ := json.Marshal(map[string]any{"doctype": "Sale", "name": name, "data": map[string]any{"status": "Void"}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.update", Payload: update, Context: opCtx(s.Name, "Administrator")}))
		expectLedger(t, s,
			ledgerRow{Product: p[0], Change: -3, Balance: 7, SourceDoctype: "Sale", SourceName: name, Reason: "Paid", Owner: "tester"},
			ledgerRow{Product: p[0], Change: 3, Balance: 10, SourceDoctype: "Sale", SourceName: name, Reason: "Void", Owner: "tester"},
		)
	})
}

func TestPackCommandWritesLedgerRowsAndAReplayWritesNone(t *testing.T) {
	forEachLedgerDialect(t, func(t *testing.T, s *siteFixture, k *kernel.Kernel) {
		mustRegister(t, k, recordSaleYAML)
		mustRegister(t, k, setSaleStatusYAML)
		p := seedLedgerProducts(t, s, k, 10)
		recordSale := func(status, key string) contract.CommandResult {
			payload, _ := json.Marshal(map[string]any{"data": map[string]any{"sale_status": status, "item_product": p[0], "item_quantity": 3}})
			op := kernel.Operation{Command: "pos.record_sale", Payload: payload, Context: opCtx(s.Name, "Administrator")}
			op.Context.IdempotencyKey = key
			return exec(t, s, k, op)
		}
		mustComplete(t, recordSale("Paid", "ledger-sale-1"))
		name := latestName(t, s, "Sale")
		want := ledgerRow{Product: p[0], Change: -3, Balance: 7, SourceDoctype: "Sale", SourceName: name, Reason: "Paid", Owner: "tester"}
		expectLedger(t, s, want)

		retried := recordSale("Paid", "ledger-sale-1")
		mustComplete(t, retried)
		if !retried.Replayed {
			t.Fatalf("keyed retry was not a replay: %+v", retried)
		}
		expectLedger(t, s, want)

		payload, _ := json.Marshal(map[string]any{"data": map[string]any{"sale": name, "sale_status": "Void"}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "pos.set_sale_status", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
		expectLedger(t, s, want, ledgerRow{Product: p[0], Change: 3, Balance: 10, SourceDoctype: "Sale", SourceName: name, Reason: "Void", Owner: "tester"})
	})
}

func TestLedgerRowsCannotBeChangedOrDeleted(t *testing.T) {
	forEachLedgerDialect(t, func(t *testing.T, s *siteFixture, k *kernel.Kernel) {
		p := seedLedgerProducts(t, s, k, 10)
		payload, _ := json.Marshal(map[string]any{"doctype": "Sale", "data": map[string]any{"status": "Paid", "product": p[0], "quantity": 3}})
		mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
		before := ledgerRows(t, s)
		row := latestLedgerName(t, s)

		update, _ := json.Marshal(map[string]any{"doctype": "Stock Movement", "name": row, "data": map[string]any{"change": 99}})
		if res := exec(t, s, k, kernel.Operation{Command: "record.update", Payload: update, Context: opCtx(s.Name, "Administrator")}); res.Status == contract.StatusCompleted {
			t.Fatal("a ledger row was changed")
		}
		del, _ := json.Marshal(map[string]any{"doctype": "Stock Movement", "name": row})
		if res := exec(t, s, k, kernel.Operation{Command: "record.delete", Payload: del, Context: opCtx(s.Name, "Administrator")}); res.Status == contract.StatusCompleted {
			t.Fatal("a ledger row was deleted")
		}
		if got := ledgerRows(t, s); !reflect.DeepEqual(got, before) {
			t.Fatalf("ledger changed after refused edits\n got: %+v\nwant: %+v", got, before)
		}
	})
}

func latestLedgerName(t *testing.T, s *siteFixture) string {
	t.Helper()
	return latestName(t, s, "Stock Movement")
}

// forEachLedgerDialect runs one check on a fresh ledger fixture per dialect.
func forEachLedgerDialect(t *testing.T, check func(t *testing.T, s *siteFixture, k *kernel.Kernel)) {
	t.Helper()
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			s := ledgerFixture(t, tc.driver, tc.env)
			defer s.DB.Close()
			k := newKernel(s)
			k.Commands = kernel.NewCommandRegistry()
			check(t, s, k)
		})
	}
}
