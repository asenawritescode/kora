package doctype

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestPOSPackageValidatesAndInstalls(t *testing.T) {
	root := filepath.Join("..", "..", "config", "pos")
	doctypes, err := ParseConfigTree(root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	inv, err := ParseConfigTree(filepath.Join("..", "..", "config", "inventory"))
	if err != nil {
		t.Fatalf("parse inventory dependency: %v", err)
	}
	if errs := ValidateAll(append(doctypes, inv...)); len(errs) > 0 {
		t.Fatalf("cross-file validation: %v", errs)
	}
	reg := NewRegistry()
	for _, dt := range doctypes {
		if dt.Module != "POS" {
			t.Errorf("%s module = %q, want POS", dt.Name, dt.Module)
		}
		reg.Register(dt)
	}
	for _, name := range []string{"Register", "Shift", "Till Session", "Sale", "Sale Item", "Sale Payment"} {
		if reg.Get(name) == nil {
			t.Errorf("registry missing %s", name)
		}
	}
	if got := reg.Get("Sale").NamingSeries; got != "SALE-.YYYY.-.#####" {
		t.Errorf("Sale naming_series = %q", got)
	}
}

func TestTillSessionVarianceComputed(t *testing.T) {
	dts, err := ParseConfigTree(filepath.Join("..", "..", "config", "pos"))
	if err != nil {
		t.Fatal(err)
	}
	var dt *DocType
	for _, d := range dts {
		if d.Name == "Till Session" {
			dt = d
		}
	}
	for _, c := range []struct {
		count, expected float64
		want            string
	}{{1050, 1000, "50"}, {990, 1000, "-10"}, {1000, 1000, "0"}} {
		doc := &Document{DocType: "Till Session", Fields: map[string]any{"closing_count": c.count, "expected_cash": c.expected}}
		if err := ComputeFields(dt, doc); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(doc.Fields["variance"]); got != c.want {
			t.Errorf("variance(%v,%v) = %#v, want %v", c.count, c.expected, doc.Fields["variance"], c.want)
		}
	}
}

// TestSalePaymentReferenceIsOptional (#336): cash refunds have no payment
// reference; record_sale still demands one through its payment guard.
func TestSalePaymentReferenceIsOptional(t *testing.T) {
	dts, err := ParseConfigTree(filepath.Join("..", "..", "config", "pos"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dts {
		if d.Name != "Sale Payment" {
			continue
		}
		for _, f := range d.Fields {
			if f.Fieldname == "reference" && f.Reqd {
				t.Fatal("Sale Payment.reference must not be required")
			}
		}
		return
	}
	t.Fatal("Sale Payment missing")
}

// The shipped register must offer both tenders so Studio can show the manual
// mobile-money option. Studio still hides it for tenants without an enabled
// Payment Account, so cash-only tenants are unaffected.
func TestPOSRegisterTenderOffersCashAndMobileMoney(t *testing.T) {
	view, err := ParseViewFile(filepath.Join("..", "..", "config", "pos", "views", "register.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var tender *ViewComponent
	for i := range view.Components {
		if view.Components[i].Type == "tender_panel" {
			tender = &view.Components[i]
		}
	}
	if tender == nil {
		t.Fatal("register view has no tender_panel component")
	}
	if got, want := tender.Bindings["methods"], "literal:Cash,Mobile Money"; got != want {
		t.Errorf("tender methods = %q, want %q", got, want)
	}
}
