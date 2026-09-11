package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func descriptor(namespace, name string, version int, kind contract.ResourceKind, fields []contract.TypedField) contract.ResourceDescriptor {
	return contract.ResourceDescriptor{
		Ref:    contract.ResourceRef{Namespace: namespace, Name: name, Version: version},
		Kind:   kind,
		Fields: fields,
	}
}

func TestResourceRegistryRegisterVersionMonotonic(t *testing.T) {
	reg := NewMemory()
	ctx := context.Background()

	first := descriptor("tenant-a", "animal", 0, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}})
	r1, err := reg.Register(ctx, first)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if r1.Version != 1 {
		t.Fatalf("first version = %d, want 1", r1.Version)
	}

	// Same (namespace, name), different content → version bumps to 2.
	second := descriptor("tenant-a", "animal", 0, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}, {Name: "breed", Type: "Data"}})
	r2, err := reg.Register(ctx, second)
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if r2.Version != 2 {
		t.Fatalf("second version = %d, want 2", r2.Version)
	}

	d1, _ := reg.Resolve(contract.ResourceRef{Namespace: "tenant-a", Name: "animal", Version: 1})
	d2, _ := reg.Resolve(contract.ResourceRef{Namespace: "tenant-a", Name: "animal", Version: 2})
	if d1.Hash == d2.Hash {
		t.Fatalf("hash did not change across versions")
	}
}

func TestResourceRegistryIdempotentReRegister(t *testing.T) {
	reg := NewMemory()
	ctx := context.Background()

	d := descriptor("tenant-a", "animal", 3, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}})
	r1, err := reg.Register(ctx, d)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	r2, err := reg.Register(ctx, d)
	if err != nil {
		t.Fatalf("re-register same content: %v", err)
	}
	if r1.Version != r2.Version || r1.String() != r2.String() {
		t.Fatalf("idempotent re-register changed identity: %v vs %v", r1, r2)
	}
}

func TestResourceRegistryVersionConflict(t *testing.T) {
	reg := NewMemory()
	ctx := context.Background()

	d1 := descriptor("tenant-a", "animal", 5, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}})
	if _, err := reg.Register(ctx, d1); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Same version, different content → conflict.
	d2 := descriptor("tenant-a", "animal", 5, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}, {Name: "breed", Type: "Data"}})
	if _, err := reg.Register(ctx, d2); !errors.Is(err, contract.ErrResourceVersionConflict) {
		t.Fatalf("want ErrResourceVersionConflict, got %v", err)
	}
}

func TestResourceIdentityNamespaceIsolation(t *testing.T) {
	reg := NewMemory()
	ctx := context.Background()

	// Two tenants register the same name; they must never collide.
	a := descriptor("tenant-a", "animal", 0, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}})
	b := descriptor("tenant-b", "animal", 0, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}})

	ra, err := reg.Register(ctx, a)
	if err != nil {
		t.Fatalf("tenant-a register: %v", err)
	}
	rb, err := reg.Register(ctx, b)
	if err != nil {
		t.Fatalf("tenant-b register: %v", err)
	}
	if ra.Version != 1 || rb.Version != 1 {
		t.Fatalf("cross-tenant registration interfered: a=%v b=%v", ra, rb)
	}

	// Listing tenant-a must not leak tenant-b descriptors.
	listed, _ := reg.List("tenant-a", "")
	if len(listed) != 1 || listed[0].Ref.Namespace != "tenant-a" {
		t.Fatalf("namespace isolation leak: %v", listed)
	}
}

func TestResolveUnknownNamespaceTypedError(t *testing.T) {
	reg := NewMemory()
	_, err := reg.Resolve(contract.ResourceRef{Namespace: "missing", Name: "animal", Version: 1})
	if !errors.Is(err, contract.ErrResourceNotFound) {
		t.Fatalf("want ErrResourceNotFound, got %v", err)
	}
}

func TestResolveLatestWhenVersionZero(t *testing.T) {
	reg := NewMemory()
	ctx := context.Background()
	for _, v := range []int{0, 0} {
		if _, err := reg.Register(ctx, descriptor("tenant-a", "animal", v, contract.ResourceKindDoctype, nil)); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	d, err := reg.Resolve(contract.ResourceRef{Namespace: "tenant-a", Name: "animal"})
	if err != nil {
		t.Fatalf("resolve latest: %v", err)
	}
	if d.Ref.Version != 2 {
		t.Fatalf("latest version = %d, want 2", d.Ref.Version)
	}
}

func TestCanonicalHashStableAcrossKeyOrder(t *testing.T) {
	a := map[string]any{"name": "animal", "kind": "doctype", "version": 1}
	b := map[string]any{"version": 1, "kind": "doctype", "name": "animal"}
	if CanonicalHash(a) != CanonicalHash(b) {
		t.Fatalf("canonical hash differs across map key order")
	}

	// Descriptor hashing must be stable across repeated serialization.
	d := descriptor("tenant-a", "animal", 1, contract.ResourceKindDoctype,
		[]contract.TypedField{{Name: "name", Type: "Data"}})
	if DescriptorHash(d) != DescriptorHash(d) {
		t.Fatalf("descriptor hash not stable")
	}
}

func TestRegisterRejectsEmptyIdentity(t *testing.T) {
	reg := NewMemory()
	ctx := context.Background()
	if _, err := reg.Register(ctx, descriptor("", "animal", 1, contract.ResourceKindDoctype, nil)); !errors.Is(err, contract.ErrResourceNamespaceRequired) {
		t.Fatalf("want ErrResourceNamespaceRequired, got %v", err)
	}
	if _, err := reg.Register(ctx, descriptor("tenant-a", "", 1, contract.ResourceKindDoctype, nil)); !errors.Is(err, contract.ErrResourceNameRequired) {
		t.Fatalf("want ErrResourceNameRequired, got %v", err)
	}
}

func TestResourceRegistryRejectsUnknownKind(t *testing.T) {
	reg := NewMemory()
	_, err := reg.Register(context.Background(), descriptor("tenant-a", "thing", 1, contract.ResourceKind("mystery"), nil))
	if !errors.Is(err, contract.ErrResourceKindUnknown) {
		t.Fatalf("want ErrResourceKindUnknown, got %v", err)
	}
}
