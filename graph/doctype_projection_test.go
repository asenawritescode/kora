package graph

import (
	"context"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

func TestProjectDocTypesUsesCanonicalSchemaAndRevision(t *testing.T) {
	source := doctype.NewRegistry()
	source.Register(&doctype.DocType{
		Name:   "Stock Movement",
		Fields: []doctype.Field{{Fieldname: "quantity", Fieldtype: "Float", Label: "Quantity", Reqd: true, Unique: true}},
	})
	target := NewMemory()
	refs, err := ProjectDocTypes(context.Background(), target, source, "tenant-a", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "Stock Movement" || refs[0].Version != 7 {
		t.Fatalf("unexpected projected refs: %#v", refs)
	}
	descriptor, err := target.Resolve(contract.ResourceRef{Namespace: "tenant-a", Name: "Stock Movement", Version: 7})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Kind != contract.ResourceKindDoctype || len(descriptor.Fields) != 1 || !descriptor.Fields[0].Required || !descriptor.Fields[0].Unique {
		t.Fatalf("projection did not preserve canonical DocType fields: %#v", descriptor)
	}
	if _, err := target.Resolve(contract.ResourceRef{Namespace: "tenant-a", Name: "Stock Movement", Version: 8}); err == nil {
		t.Fatal("projection created an unexpected independent resource version")
	}
}

func TestProjectDocTypesIsIdempotentForSameModelRevision(t *testing.T) {
	source := doctype.NewRegistry()
	source.Register(&doctype.DocType{Name: "Item"})
	target := NewMemory()
	ctx := context.Background()
	first, err := ProjectDocTypes(ctx, target, source, "tenant-a", 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectDocTypes(ctx, target, source, "tenant-a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != second[0] {
		t.Fatalf("same model revision changed graph identity: %v vs %v", first, second)
	}
}
