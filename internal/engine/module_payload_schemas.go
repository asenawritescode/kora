package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/workflow"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// modulePayloadSchemas adapts persisted module schema documents to the existing
// workflow payload registry. Each runtime generation owns its compiled schemas.
type modulePayloadSchemas struct {
	base    workflow.PayloadSchemaRegistry
	schemas map[contract.SchemaRef]*jsonschema.Schema
}

func newModulePayloadSchemas(modules []contract.ModuleDescriptor, base workflow.PayloadSchemaRegistry) (workflow.PayloadSchemaRegistry, error) {
	if err := contract.ValidateModuleSet(modules, nil); err != nil {
		return nil, err
	}
	result := &modulePayloadSchemas{base: base, schemas: make(map[contract.SchemaRef]*jsonschema.Schema)}
	documents := make(map[contract.SchemaRef][]byte)
	for _, module := range modules {
		for _, document := range module.Schemas {
			var value any
			if err := json.Unmarshal(document.Schema, &value); err != nil {
				return nil, err
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			if previous, exists := documents[document.Ref]; exists {
				if !bytes.Equal(previous, canonical) {
					return nil, fmt.Errorf("conflicting module schema %s@%d", document.Ref.ID, document.Ref.Version)
				}
				continue
			}
			documents[document.Ref] = canonical
			compiler := jsonschema.NewCompiler()
			location := "https://kora.invalid/module-schema.json"
			if err := compiler.AddResource(location, value); err != nil {
				return nil, err
			}
			compiled, err := compiler.Compile(location)
			if err != nil {
				return nil, fmt.Errorf("compile module schema %s: %w", document.Ref.ID, err)
			}
			result.schemas[document.Ref] = compiled
		}
	}
	return result, nil
}

func (r *modulePayloadSchemas) Registered(ref contract.SchemaRef) bool {
	return r.schemas[ref] != nil || r.base != nil && r.base.Registered(ref)
}

func (r *modulePayloadSchemas) Validate(ref contract.SchemaRef, raw json.RawMessage) error {
	schema := r.schemas[ref]
	if schema == nil {
		if r.base != nil {
			return r.base.Validate(ref, raw)
		}
		return fmt.Errorf("module schema %s is not registered", ref.ID)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("payload must contain one JSON value")
	}
	if err := schema.Validate(value); err != nil {
		return err
	}
	if r.base != nil && r.base.Registered(ref) {
		return r.base.Validate(ref, raw)
	}
	return nil
}
