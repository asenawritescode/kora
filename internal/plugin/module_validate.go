package plugin

import (
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
)

func validReconciliationRegistrationName(value string) bool {
	if len(value) == 0 || len(value) > 128 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value[1:] {
		if !(char == '_' || char == '-' || char == '.' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func validBusinessQueryRegistration(owner contract.ModuleID, name string, input json.RawMessage, pageLimit uint16) bool {
	return owner != "" && validReconciliationRegistrationName(name) && len(input) > 0 && len(input) <= 65536 && json.Valid(input) && pageLimit > 0 && pageLimit <= 1000
}

func queryAllowsBusinessFields(query contract.PublishedQuery, source bool) bool {
	fields := make(map[string]bool, len(query.AllowedFields))
	for _, field := range query.AllowedFields {
		fields[field] = true
	}
	required := []string{"key", "entity_id", "amount", "currency", "occurred_at"}
	if source {
		required = append(required, "source_ref")
	} else {
		required = append(required, "target_ref")
	}
	for _, field := range required {
		if !fields[field] {
			return false
		}
	}
	return true
}

func moduleDeclaresOffer(module contract.ModuleDescriptor, offer contract.CapabilityOffer) bool {
	for _, declared := range module.Provides {
		if declared.Name != offer.Name || declared.Contract != offer.Contract || len(declared.Operations) != len(offer.Operations) {
			continue
		}
		declaredOps := make(map[string]contract.OperationSpec, len(declared.Operations))
		for _, operation := range declared.Operations {
			declaredOps[operation.Name] = operation
		}
		matches := true
		for _, operation := range offer.Operations {
			if expected, ok := declaredOps[operation.Name]; !ok || expected != operation {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

// validateLineEntryLayouts resolves each line_entry lookup query across modules.
func validateLineEntryLayouts(modules []contract.ModuleDescriptor) error {
	queries := map[string]contract.PublishedQuery{}
	for _, m := range modules {
		for _, q := range m.Queries {
			queries[contract.PublishedOperationRef(q.OwnerModuleID, q.Capability, q.Operation)] = q
		}
	}
	for _, m := range modules {
		schemas := map[contract.SchemaRef]json.RawMessage{}
		for _, s := range m.Schemas {
			schemas[s.Ref] = s.Schema
		}
		for _, po := range m.PublishedOperations {
			le := po.Presentation.LineEntry
			if po.Presentation.Layout != "line_entry" || le == nil {
				continue
			}
			q, ok := queries[le.LookupQuery]
			if !ok || q.ScopePolicy != po.ScopePolicy && q.ScopePolicy != contract.ScopeEntityOwned {
				return fmt.Errorf("%s: module %q operation %q lookup_query is missing or has a wider scope", contract.ErrLayoutInvalid, m.ID, po.Name)
			}
			var op *contract.OperationSpec
			for _, offer := range m.Provides {
				if offer.Name != po.Capability {
					continue
				}
				for i := range offer.Operations {
					if offer.Operations[i].Name == po.Operation {
						op = &offer.Operations[i]
					}
				}
			}
			if op == nil {
				return fmt.Errorf("%s: module %q operation %q is not declared", contract.ErrLayoutInvalid, m.ID, po.Name)
			}
			if err := le.Validate(schemas[op.Input], schemas[op.Output], q.AllowedFields); err != nil {
				return fmt.Errorf("module %q operation %q: %w", m.ID, po.Name, err)
			}
		}
	}
	return nil
}
