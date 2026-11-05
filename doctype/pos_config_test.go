package doctype

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestPOSPackageValidatesAndInstalls(t *testing.T) {
	root := filepath.Join("..", "config", "pos")
	doctypes, err := ParseConfigTree(root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	inv, err := ParseConfigTree(filepath.Join("..", "config", "inventory"))
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
	dts, err := ParseConfigTree(filepath.Join("..", "config", "pos"))
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
