package site

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/query"
)

var (
	packSemver     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	packMajorMinor = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))?$`)
)

// PackModuleVersion makes a pack version semver ("1" -> "1.0.0").
func PackModuleVersion(v string) (string, error) {
	switch {
	case packSemver.MatchString(v):
		return v, nil
	case packMajorMinor.MatchString(v) && strings.Contains(v, "."):
		return v + ".0", nil
	case packMajorMinor.MatchString(v):
		return v + ".0.0", nil
	}
	return "", fmt.Errorf("pack.version_invalid: pack version %q cannot be made semver", v)
}

// GeneratePackModules derives the <pack>.core owner and <pack>.app caller
// descriptors from the pack's config queries. It is the only producer.
func GeneratePackModules(manifest contract.PackManifest, snapshot *doctype.ConfigSnapshot) ([]contract.ModuleDescriptor, error) {
	version, err := PackModuleVersion(manifest.Version)
	if err != nil {
		return nil, err
	}
	owner := contract.ModuleID(manifest.Name + ".core")
	core := contract.ModuleDescriptor{ID: owner, Version: version}
	offers := map[contract.CapabilityName]*contract.CapabilityOffer{}
	var names []contract.CapabilityName
	if snapshot != nil {
		for _, raw := range snapshot.Queries {
			var def query.Definition
			if err := json.Unmarshal(raw, &def); err != nil {
				return nil, fmt.Errorf("pack.query_invalid: %w", err)
			}
			if def.ModuleOwnerID != string(owner) {
				return nil, fmt.Errorf("pack.module_owner_mismatch: query %q module_owner_id %q must be %q", def.Name, def.ModuleOwnerID, owner)
			}
			if def.Name != def.Operation {
				return nil, fmt.Errorf("pack.query_invalid: query name %q must equal operation %q", def.Name, def.Operation)
			}
			in := contract.SchemaRef{ID: fmt.Sprintf("%s.%s.query.input", owner, def.Operation), Version: 1}
			out := contract.SchemaRef{ID: fmt.Sprintf("%s.%s.query.output", owner, def.Operation), Version: 1}
			limit := def.Limit
			if limit == 0 {
				limit = 50
			}
			fields := def.OutputFields()
			scope := contract.ScopeTenantShared
			if def.Scope == "legal_entity" {
				scope = contract.ScopeEntityOwned
			}
			pq := contract.PublishedQuery{OwnerModuleID: owner, Name: def.Operation, Capability: contract.CapabilityName(def.Capability), Operation: def.Operation,
				InputSchema: in, OutputSchema: out, ScopePolicy: scope, AllowedFields: fields, OwnerField: def.OwnerField, PageLimit: uint16(limit),
				Presentation: contract.SurfacePresentation{Title: def.Presentation["title"], Description: def.Presentation["description"], Icon: def.Presentation["icon"], Form: def.Presentation["form"], List: def.Presentation["list"]}}
			for _, f := range fields {
				if f == "name" && def.OfflineEligible {
					pq.OfflineEligible, pq.OfflineFactKey = true, "name"
				}
			}
			core.Queries = append(core.Queries, pq)
			core.Schemas = append(core.Schemas,
				contract.ModuleSchemaDocument{Ref: in, Schema: querySchema(def)},
				contract.ModuleSchemaDocument{Ref: out, Schema: queryOutputSchema(fields)})
			o := offers[pq.Capability]
			if o == nil {
				o = &contract.CapabilityOffer{Name: pq.Capability, Contract: contract.SchemaRef{ID: def.Capability + ".contract", Version: 1}}
				offers[pq.Capability] = o
				names = append(names, pq.Capability)
			}
			o.Operations = append(o.Operations, contract.OperationSpec{Name: def.Operation, Input: in, Output: out, Effect: contract.EffectQuery})
		}
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	for _, n := range names {
		core.Provides = append(core.Provides, *offers[n])
	}
	return []contract.ModuleDescriptor{core, {ID: contract.ModuleID(manifest.Name + ".app"), Version: version}}, nil
}

func querySchema(def query.Definition) json.RawMessage {
	props := map[string]any{}
	for name, spec := range def.Input {
		p := map[string]any{"type": map[string]string{"string": "string", "int": "integer", "number": "number", "bool": "boolean"}[spec.Type]}
		if spec.MaxLength > 0 {
			p["maxLength"] = spec.MaxLength
		}
		if len(spec.Enum) > 0 {
			p["enum"] = spec.Enum
		}
		props[name] = p
	}
	raw, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "additionalProperties": false})
	return raw
}

func queryOutputSchema(fields []string) json.RawMessage {
	props := map[string]any{}
	for _, f := range fields {
		props[f] = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": props}})
	return raw
}
