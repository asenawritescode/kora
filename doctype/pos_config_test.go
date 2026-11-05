package doctype

import (
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
