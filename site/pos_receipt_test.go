package site

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/print"
)

// TestPOSReceiptsShowItemNames (#337): both receipt formats print the item's
// title (the "<link field>_title" print data), never the raw item record id.
func TestPOSReceiptsShowItemNames(t *testing.T) {
	for _, file := range []string{"sale_receipt.yaml", "sale_record_receipt.yaml"} {
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile("../config/pos/print_formats/" + file)
			if err != nil {
				t.Fatal(err)
			}
			f, err := print.Load(body)
			if err != nil {
				t.Fatal(err)
			}
			data := print.Data{
				Doc: map[string]any{"name": "SALE-2026-00001", "total": 425.0,
					"items": []any{
						map[string]any{"item": "ITEM-0001", "item_title": "Maize Flour 2kg", "quantity": 2, "amount": 360.0},
						map[string]any{"item": "ITEM-0004", "item_title": "Bread 400g", "quantity": 1, "amount": 65.0}},
					"payments": []any{map[string]any{"method": "Cash", "amount": 425.0}}},
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
			for _, out := range []string{txt, h} {
				for _, w := range []string{"Maize Flour 2kg", "Bread 400g"} {
					if !strings.Contains(out, w) {
						t.Errorf("missing %q\n%s", w, out)
					}
				}
				for _, id := range []string{"ITEM-0001", "ITEM-0004"} {
					if strings.Contains(out, id) {
						t.Errorf("record id %q printed\n%s", id, out)
					}
				}
			}
		})
	}
}

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
			"items":    []any{map[string]any{"item": "T", "item_title": "Tea", "quantity": 2, "amount": 200.0}, map[string]any{"item": "B", "item_title": "Bread", "quantity": 1, "amount": 150.0}},
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
	data := print.Data{
		Doc: map[string]any{"name": "TILL-1", "opening_float": 500.0, "expected_cash": 700.0, "closing_count": 690.0, "variance": -10.0},
		Queries: map[string][]map[string]any{
			"by_method": {{"method": "Cash", "payment_count": 2, "total": 300.0}, {"method": "Card", "payment_count": 1, "total": 200.0}},
			"refunds":   {{"method": "Cash", "payment_count": 1, "total": 100.0}},
			"by_account": {{"payment_account": "PA-0001", "verification": "manually_recorded", "method": "Mobile Money", "payment_count": 1, "total": 120.0},
				{"payment_account": "PA-0002", "verification": "provider_confirmed", "method": "Mobile Money", "payment_count": 2, "total": 80.0}},
		},
		Tenant: map[string]any{"name": "Shop"},
		Now:    time.Unix(0, 0).UTC(),
	}
	var outs []string
	for _, file := range []string{"z_report.yaml", "z_report_close_session.yaml", "z_report_reprint.yaml"} {
		body, err := os.ReadFile("../config/pos/print_formats/" + file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := print.Load(body)
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Queries) != 3 {
			t.Fatalf("%s queries = %v", file, f.Queries)
		}
		txt, err := f.RenderText(data, print.Options{Currency: "KES", Precision: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []string{"Z REPORT", "TILL-1", "KES 300.00", "KES 200.00", "REFUNDS", "KES 100.00", "KES 500.00", "KES 700.00", "KES 690.00", "VARIANCE", "-10.00", "BY ACCOUNT", "PA-0001", "KES 120.00", "Mobile Money - manually recorded", "PA-0002"} {
			if !strings.Contains(txt, w) {
				t.Errorf("%s missing %q\n%s", file, w, txt)
			}
		}
		outs = append(outs, txt)
	}
	for i, o := range outs[1:] {
		if o != outs[0] {
			t.Errorf("format %d prints differently from z_report:\n%s\n---\n%s", i+1, o, outs[0])
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
			"items":    []any{map[string]any{"item": "T", "item_title": "Tea", "quantity": 2, "amount": 200.0}, map[string]any{"item": "B", "item_title": "Bread", "quantity": 1, "amount": 150.0}},
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
