package site

import (
	"fmt"
	"sort"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// addPackOwnership publishes every DocType that declares module_owner_id as
// an OwnedResource of core. Child tables inherit their parent's ownership.
func addPackOwnership(core *contract.ModuleDescriptor, snapshot *doctype.ConfigSnapshot) error {
	type decl struct{ owner, mode, access string }
	byName := map[string]*doctype.DocType{}
	for _, dt := range snapshot.DocTypes {
		if dt != nil {
			byName[dt.Name] = dt
		}
	}
	own := func(dt *doctype.DocType) decl {
		mode, access := dt.ModuleOwnershipMode, string(dt.ModuleAccess)
		if mode == "" {
			mode = string(contract.MutationCRUD)
		}
		if access == "" {
			access = string(contract.ModulePrivate)
		}
		return decl{dt.ModuleOwnerID, mode, access}
	}
	resolved := map[string]decl{}
	var names []string
	for name, dt := range byName {
		if dt.ModuleOwnerID == "" && dt.ModuleOwnershipMode == "" && dt.ModuleAccess == "" {
			continue
		}
		d := own(dt)
		if d.owner != string(core.ID) {
			return fmt.Errorf("pack.module_owner_mismatch: doctype %q module_owner_id %q must be %q", name, d.owner, core.ID)
		}
		if d.mode != string(contract.MutationCRUD) && d.mode != string(contract.MutationCommands) {
			return fmt.Errorf("pack.module_ownership_mode_invalid: doctype %q mode %q must be crud or commands_only", name, d.mode)
		}
		if d.access != string(contract.ModulePrivate) && d.access != string(contract.ModulePublished) {
			return fmt.Errorf("pack.module_access_invalid: doctype %q module_access %q must be private or published", name, d.access)
		}
		resolved[name] = d
	}
	// Children inherit from every owned parent; parents must agree.
	parentOf := map[string]string{}
	for _, name := range sortedKeys(resolved) {
		for _, f := range byName[name].Fields {
			child := byName[f.Options]
			if f.Fieldtype != "Table" || child == nil {
				continue
			}
			if prev, ok := parentOf[child.Name]; ok && resolved[prev] != resolved[name] {
				return fmt.Errorf("pack.child_owner_conflict: child %q is used by %q and %q with different ownership", child.Name, prev, name)
			}
			if d, ok := resolved[child.Name]; ok && child.ModuleOwnerID != "" && d != resolved[name] {
				return fmt.Errorf("pack.child_owner_conflict: child %q ownership differs from parent %q", child.Name, name)
			}
			parentOf[child.Name] = name
		}
	}
	for child, parent := range parentOf {
		resolved[child] = resolved[parent]
	}
	for name := range resolved {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d, dt := resolved[name], byName[name]
		scope := dt.BusinessScopePolicy
		if scope == "" {
			scope = contract.ScopeTenantShared
		}
		core.Resources = append(core.Resources, contract.OwnedResource{
			Ref: contract.ResourceRef{Namespace: "tenant", Name: name, Version: 1}, Access: contract.ModuleAccess(d.access),
			MutationMode: contract.MutationMode(d.mode), ScopeKind: scope, ScopeField: dt.BusinessScopeField})
	}
	return nil
}

// applyPackOwnership writes the generated descriptor's ownership onto the
// snapshot's DocTypes (children included), as Cloud install does.
func applyPackOwnership(snapshot *doctype.ConfigSnapshot, modules []contract.ModuleDescriptor) {
	for _, m := range modules {
		for _, r := range m.Resources {
			for _, dt := range snapshot.DocTypes {
				if dt != nil && r.Ref.Namespace == "tenant" && dt.Name == r.Ref.Name {
					dt.ModuleOwnerID, dt.ModuleAccess, dt.ModuleOwnershipMode = string(m.ID), r.Access, string(r.MutationMode)
					dt.BusinessScopePolicy, dt.BusinessScopeField = r.ScopeKind, r.ScopeField
				}
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
