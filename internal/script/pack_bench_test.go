package script

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// packConfigRoot is the config directory holding the real pack sources, relative to this package.
const packConfigRoot = "../../config/_parked"

// packSource reads one real script file from a pack and fails the benchmark when it is missing.
func packSource(b *testing.B, rel ...string) string {
	b.Helper()
	src, err := os.ReadFile(filepath.Join(append([]string{packConfigRoot}, rel...)...))
	if err != nil {
		b.Fatalf("read pack source: %v", err)
	}
	return string(src)
}

// packBooksProvider answers the reads the Sale posting hook makes from a fixed set of
// records: one Active account per posting role the hook uses, one open period covering the
// sale and the cost price of each product. It accepts the writes (journal entries, customer
// saves) and counts them. It is not a database: filters are not evaluated.
type packBooksProvider struct {
	NoopProvider
	products map[string]map[string]any
	accounts []map[string]any
	period   map[string]any
	created  int
	saved    int
}

// newPackBooksProvider seeds the records the Sale hook reads for a sale over products.
func newPackBooksProvider(products int) *packBooksProvider {
	p := &packBooksProvider{products: make(map[string]map[string]any, products)}
	for _, role := range []string{"Cash", "Mobile Money", "Sales", "Tax Payable", "Cost of Sales", "Inventory"} {
		p.accounts = append(p.accounts, map[string]any{"name": "ACC-" + role, "posting_role": role, "status": "Active"})
	}
	p.period = map[string]any{"name": "PER-2026-11", "start_date": "2026-11-01", "end_date": "2026-11-30", "status": "Open"}
	for i := range products {
		name := fmt.Sprintf("PRD-%02d", i)
		p.products[name] = map[string]any{"name": name, "cost_price": 6.0}
	}
	return p
}

func (p *packBooksProvider) HasDocType(name string) bool {
	return name == "Journal Entry" || name == "Account"
}

func (p *packBooksProvider) GetDoc(doctype, name string) (map[string]any, error) {
	return p.products[name], nil
}

func (p *packBooksProvider) GetList(doctype string, _ map[string]any, _ string, limit, _ int) ([]map[string]any, error) {
	switch doctype {
	case "Account":
		return p.accounts, nil
	case "Accounting Period":
		return []map[string]any{p.period}, nil
	}
	return nil, nil // no posted entry yet
}

func (p *packBooksProvider) SaveDoc(string, map[string]any, string) error {
	p.saved++
	return nil
}

func (p *packBooksProvider) CreateDoc(doctype string, doc map[string]any, _, _ string) (map[string]any, error) {
	p.created++
	doc["name"] = fmt.Sprintf("JE-%d", p.created)
	return doc, nil
}

// packSale is a register sale paid in cash and mobile money, with lines to price and cost.
func packSale(lines int) map[string]any {
	items := make([]any, 0, lines)
	for i := range lines {
		items = append(items, map[string]any{
			"product":         fmt.Sprintf("PRD-%02d", i),
			"quantity":        2,
			"unit_price":      10.5,
			"discount_amount": 0,
			"tax_amount":      1.5,
		})
	}
	return map[string]any{
		"name": "SAL-0001", "reference": "SAL-0001", "customer": "CUST-1",
		"status": "Paid", "on_credit": 0, "sale_date": "2026-11-27", "exchange_rate": 1,
		"items": items,
		"payments": []any{
			map[string]any{"method": "Cash", "amount": 80.0},
			map[string]any{"method": "Mobile Money", "amount": 32.5},
		},
	}
}

// BenchmarkPackScripts runs the real erpsell Sale posting hook with the real erp-accounting
// books library (lib_books), the way a pack event does. It needs no tag: goja with
// -tags gojascript, QuickJS by default.
func BenchmarkPackScripts(b *testing.B) {
	libBooks := Library{Name: "lib_books", Source: packSource(b, "erp-accounting", "scripts", "lib", "books.js")}
	hook := packSource(b, "erpsell", "scripts", "Sale", "before_save.js")
	sale := packSale(5)
	cases := []struct {
		name string
		req  ExecuteRequest
	}{
		{name: "posting_sale", req: ExecuteRequest{
			Script:    hook,
			DocType:   "Sale",
			Event:     EventBeforeSave,
			Document:  sale,
			Libraries: []Library{libBooks},
		}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			r := benchRunner(b)
			provider := newPackBooksProvider(5)
			req := tc.req
			req.Provider = provider
			// One checked run first: the benchmark must measure a posting that really happens.
			if _, err := r.Execute(context.Background(), req); err != nil {
				b.Fatalf("Execute: %v", err)
			}
			if provider.created != 1 {
				b.Fatalf("posting wrote %d journal entries, want 1", provider.created)
			}
			provider.created = 0
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Execute(context.Background(), req); err != nil {
					b.Fatalf("Execute: %v", err)
				}
			}
		})
	}
}
