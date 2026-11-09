package kernel

import "testing"

func TestYAMLPackageCommandsUseGenericKernelLoader(t *testing.T) {
	registry, err := LoadCommandDir("../../config/v0/inventory/commands")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inventory.add_stock", "inventory.issue_stock", "inventory.transfer_stock", "inventory.reserve_stock", "inventory.release_stock"} {
		if _, ok := registry.Lookup(name); !ok {
			t.Fatalf("missing YAML command %q", name)
		}
	}
}
