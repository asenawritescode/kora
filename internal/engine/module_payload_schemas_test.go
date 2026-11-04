package engine

import (
	"encoding/json"
	"github.com/asenawritescode/kora/contract"
	"testing"
)

func TestModulePayloadSchemasReloadAndStrictValidation(t *testing.T) {
	ref := contract.SchemaRef{ID: "acceptance.input", Version: 1}
	modules := []contract.ModuleDescriptor{{ID: "acceptance.owner", Version: "1.0.0", Schemas: []contract.ModuleSchemaDocument{{Ref: ref, Schema: json.RawMessage(`{"type":"object","required":["title"],"additionalProperties":false,"properties":{"title":{"type":"string"}}}`)}}}}
	raw, err := json.Marshal(modules)
	if err != nil {
		t.Fatal(err)
	}
	var restored []contract.ModuleDescriptor
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	for _, declarations := range [][]contract.ModuleDescriptor{modules, restored} {
		registry, err := newModulePayloadSchemas(declarations, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !registry.Registered(ref) {
			t.Fatal("persisted schema not registered")
		}
		if err := registry.Validate(ref, json.RawMessage(`{"title":"issued invoice"}`)); err != nil {
			t.Fatal(err)
		}
		for _, invalid := range []string{`{}`, `{"title":3}`, `{"title":"x","extra":true}`, `{"title":"x"} {}`} {
			if err := registry.Validate(ref, json.RawMessage(invalid)); err == nil {
				t.Fatalf("accepted invalid payload %s", invalid)
			}
		}
	}
	empty, err := newModulePayloadSchemas(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Registered(ref) || empty.Validate(ref, json.RawMessage(`{}`)) == nil {
		t.Fatal("schema leaked across runtime generations")
	}
}
