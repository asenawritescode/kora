package site

import "testing"

func TestConfigImportOperationIDIsStableAndTenantScoped(t *testing.T) {
	target := []byte(`{"doctype":"Invoice"}`)
	first := configImportOperationID("acme.example", target)
	if first == "" || len(first) != 36 {
		t.Fatalf("operation ID = %q, want 36-character digest", first)
	}
	if got := configImportOperationID("acme.example", target); got != first {
		t.Fatalf("same tenant and snapshot produced %q, want %q", got, first)
	}
	if got := configImportOperationID("other.example", target); got == first {
		t.Fatal("same snapshot for another tenant reused operation identity")
	}
	if got := configImportOperationID("acme.example", []byte(`{"doctype":"Receipt"}`)); got == first {
		t.Fatal("different snapshot reused operation identity")
	}
}
