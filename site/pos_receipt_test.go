package site

import (
	"os"
	"regexp"
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

// TestPOSZReportHTMLCarriesTheTextSections: the html render of every Z report
// format has the headings the text render has, in the same order, with
// REFUNDS and BY ACCOUNT only when there are rows, a labelled totals block, and
// no table without rows.
func TestPOSZReportHTMLCarriesTheTextSections(t *testing.T) {
	sales := []map[string]any{{"method": "Cash", "payment_count": 2, "total": 300.0}, {"method": "Card", "payment_count": 1, "total": 200.0}}
	refunds := []map[string]any{{"method": "Cash", "payment_count": 1, "total": 100.0}}
	accounts := []map[string]any{{"payment_account": "PA-0001", "verification": "manually_recorded", "method": "Mobile Money", "payment_count": 1, "total": 120.0}}
	doc := map[string]any{"name": "TILL-1", "opening_float": 500.0, "expected_cash": 700.0, "closing_count": 690.0, "variance": -10.0}
	cases := []struct {
		name                     string
		sales, refunds, accounts []map[string]any
		want, absent             []string
	}{
		{"cash only", sales[:1], nil, nil,
			[]string{"<h2>SALES</h2>"}, []string{"REFUNDS", "BY ACCOUNT", "Refund "}},
		{"mixed with accounts", sales, nil, accounts,
			[]string{"<h2>SALES</h2>", "<h2>BY ACCOUNT</h2>", "PA-0001", "(manually recorded)", `<th scope="col">Account</th>`}, []string{"REFUNDS"}},
		{"refunds", sales, refunds, nil,
			[]string{"<h2>SALES</h2>", "<h2>REFUNDS</h2>", "KES 100.00"}, []string{"BY ACCOUNT"}},
		{"everything", sales, refunds, accounts,
			[]string{"<h2>SALES</h2>", "<h2>REFUNDS</h2>", "<h2>BY ACCOUNT</h2>"}, nil},
		{"no payments at all", nil, nil, nil,
			[]string{"<h2>SALES</h2>", "No sales"}, []string{"REFUNDS", "BY ACCOUNT"}},
	}
	for _, file := range []string{"z_report.yaml", "z_report_close_session.yaml", "z_report_reprint.yaml"} {
		body, err := os.ReadFile("../config/pos/print_formats/" + file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := print.Load(body)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			t.Run(file+"/"+tc.name, func(t *testing.T) {
				data := print.Data{Doc: doc, Tenant: map[string]any{"name": "Shop"}, Now: time.Unix(0, 0).UTC(),
					Queries: map[string][]map[string]any{"by_method": tc.sales, "refunds": tc.refunds, "by_account": tc.accounts}}
				opt := print.Options{Currency: "KES", Precision: 2}
				out, err := f.RenderHTML(data, opt)
				if err != nil {
					t.Fatal(err)
				}
				txt, err := f.RenderText(data, opt)
				if err != nil {
					t.Fatal(err)
				}
				for _, w := range append([]string{
					"<h1>", "Z report",
					"<h2>Totals</h2>",
					`<th scope="row">Opening float</th><td>KES 500.00</td>`,
					`<th scope="row">Expected cash</th><td>KES 700.00</td>`,
					`<th scope="row">Counted cash</th><td>KES 690.00</td>`,
					`<th scope="row">Variance</th><td><strong>KES -10.00</strong></td>`,
				}, tc.want...) {
					if !strings.Contains(out, w) {
						t.Errorf("html missing %q\n%s", w, out)
					}
				}
				for _, w := range tc.absent {
					if strings.Contains(out, w) {
						t.Errorf("html has %q\n%s", w, out)
					}
				}
				if len(tc.sales) > 0 {
					for _, w := range []string{`<th scope="col">Method</th><th scope="col">Count</th><th scope="col">Amount</th>`, "<td>Cash</td><td>2</td><td>KES 300.00</td>"} {
						if !strings.Contains(out, w) {
							t.Errorf("html missing %q\n%s", w, out)
						}
					}
				}
				// Every section heading in the text render is a heading in the html, in order.
				at := -1
				for _, h := range []string{"SALES", "REFUNDS", "BY ACCOUNT"} {
					inText, inHTML := strings.Contains(txt, h), strings.Contains(out, "<h2>"+h+"</h2>")
					if h == "REFUNDS" && len(tc.refunds) == 0 {
						continue // the html leaves an empty section out
					}
					if inText != inHTML {
						t.Errorf("%s: text has it = %v, html has it = %v", h, inText, inHTML)
					}
					if inHTML {
						i := strings.Index(out, "<h2>"+h+"</h2>")
						if i < at {
							t.Errorf("%s out of order\n%s", h, out)
						}
						at = i
					}
				}
				// No table without rows, and the totals come after the sections.
				for _, tbl := range regexp.MustCompile(`(?s)<table.*?</table>`).FindAllString(out, -1) {
					if !strings.Contains(tbl, "<td>") {
						t.Errorf("table without rows: %s", tbl)
					}
				}
				if strings.Index(out, "<h2>Totals</h2>") < at {
					t.Errorf("totals before the sections\n%s", out)
				}
			})
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
