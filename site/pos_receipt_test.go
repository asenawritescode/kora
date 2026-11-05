package site

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/print"
)

func TestPOSSaleReceiptRenders(t *testing.T) {
	body, err := os.ReadFile("../config/pos/print_formats/sale_receipt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := print.Load(body)
	if err != nil {
		t.Fatal(err)
	}
	data := print.Data{
		Doc: map[string]any{"name": "SALE-2026-00001", "total": 350.0,
			"items":    []any{map[string]any{"item": "Tea", "quantity": 2, "amount": 200.0}, map[string]any{"item": "Bread", "quantity": 1, "amount": 150.0}},
			"payments": []any{map[string]any{"method": "Cash", "amount": 350.0}}},
		Tenant: map[string]any{"name": "Shop"},
		Now:    time.Unix(0, 0).UTC(),
	}
	opt := print.Options{Currency: "KES", Precision: 2}
	txt, err := f.RenderText(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	h, err := f.RenderHTML(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"SALE-2026-00001", "Tea", "KES 350.00", "Cash"} {
		if !strings.Contains(txt, w) || !strings.Contains(h, w) {
			t.Errorf("missing %q\n%s\n%s", w, txt, h)
		}
	}
	for _, l := range strings.Split(txt, "\n") {
		if len(l) > 48 {
			t.Errorf("line over 48 cols: %q", l)
		}
	}
}

func TestPOSZReportRenders(t *testing.T) {
	body, err := os.ReadFile("../config/pos/print_formats/z_report.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := print.Load(body)
	if err != nil {
		t.Fatal(err)
	}
	data := print.Data{
		Doc: map[string]any{"name": "TILL-1", "opening_float": 500.0, "expected_cash": 800.0, "closing_count": 790.0, "variance": -10.0,
			"payments_by_method": []any{map[string]any{"method": "Cash", "payment_count": 2, "total": 300.0}, map[string]any{"method": "Card", "payment_count": 1, "total": 50.0}}},
		Tenant: map[string]any{"name": "Shop"},
		Now:    time.Unix(0, 0).UTC(),
	}
	txt, err := f.RenderText(data, print.Options{Currency: "KES", Precision: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Z REPORT", "Cash", "Card", "KES 300.00", "VARIANCE"} {
		if !strings.Contains(txt, w) {
			t.Errorf("missing %q\n%s", w, txt)
		}
	}
}

func TestPOSSaleRecordReceiptMatchesOperationReceipt(t *testing.T) {
	load := func(file string) *print.Format {
		body, err := os.ReadFile("../config/pos/print_formats/" + file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := print.Load(body)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	op, rec := load("sale_receipt.yaml"), load("sale_record_receipt.yaml")
	if rec.For.Record != "Sale" || rec.For.Operation != "" {
		t.Fatalf("target = %+v", rec.For)
	}
	data := print.Data{
		Doc: map[string]any{"name": "SALE-2026-00001", "total": 350.0,
			"items":    []any{map[string]any{"item": "Tea", "quantity": 2, "amount": 200.0}, map[string]any{"item": "Bread", "quantity": 1, "amount": 150.0}},
			"payments": []any{map[string]any{"method": "Cash", "amount": 350.0}}},
		Tenant: map[string]any{"name": "Shop"},
		Now:    time.Unix(0, 0).UTC(),
	}
	opt := print.Options{Currency: "KES", Precision: 2}
	wantT, err := op.RenderText(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	gotT, err := rec.RenderText(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	wantH, _ := op.RenderHTML(data, opt)
	gotH, err := rec.RenderHTML(data, opt)
	if err != nil {
		t.Fatal(err)
	}
	if gotT != wantT || gotH != wantH {
		t.Errorf("record receipt differs from sale_receipt\n%s\n%s", gotT, wantT)
	}
	for _, w := range []string{"Tea", "Bread", "KES 350.00", "Cash"} {
		if !strings.Contains(gotT, w) || !strings.Contains(gotH, w) {
			t.Errorf("missing %q", w)
		}
	}
}
