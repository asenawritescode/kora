package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/asenawritescode/kora/kernel"
)

func TestOrganizationalRuntimeFixtureIsValid(t *testing.T) {
	data, err := os.ReadFile("contract-fixtures/organizational-runtime-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version       string   `json:"version"`
		ResourceKinds []string `json:"resource_kinds"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != "1" || len(fixture.ResourceKinds) < 17 {
		t.Fatalf("unexpected fixture: %+v", fixture)
	}
	if fixture.ResourceKinds[0] != "doctype" {
		t.Fatalf("canonical business schema kind = %q, want doctype", fixture.ResourceKinds[0])
	}
}

func TestYAMLPackageCommandsUseGenericKernelLoader(t *testing.T) {
	registry, err := kernel.LoadCommandDir("config/inventory/commands")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"inventory.add_stock", "inventory.issue_stock", "inventory.transfer_stock", "inventory.reserve_stock", "inventory.release_stock"} {
		if _, ok := registry.Lookup(name); !ok {
			t.Fatalf("missing YAML command %q", name)
		}
	}
}
