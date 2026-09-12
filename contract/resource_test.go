package contract

import "testing"

func TestEntityKindIsCanonicalDocTypeAlias(t *testing.T) {
	if ResourceKindEntity != ResourceKindDoctype {
		t.Fatalf("entity compatibility kind = %q, want canonical doctype %q", ResourceKindEntity, ResourceKindDoctype)
	}
}
