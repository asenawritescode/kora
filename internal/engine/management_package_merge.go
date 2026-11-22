package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/print"
	"github.com/asenawritescode/kora/internal/query"
)

func mergeManagementPackageConstraints(existing []contract.InstalledPackage, incoming contract.InstalledPackage) ([]contract.InstalledPackage, error) {
	merged := append([]contract.InstalledPackage(nil), existing...)
	for _, installed := range existing {
		if installed.ReleaseID != incoming.ReleaseID {
			continue
		}
		if !reflect.DeepEqual(installed, incoming) {
			return nil, fmt.Errorf("installed package release %q does not match the verified artifact", incoming.ReleaseID)
		}
		return merged, nil
	}
	merged = append(merged, incoming)
	sort.Slice(merged, func(i, j int) bool { return merged[i].ReleaseID < merged[j].ReleaseID })
	return merged, nil
}

// mergeManagementPackageSnapshot preserves tenant-owned configuration and
// overlays only entities named by the package. Package import is additive and
// upserting; omission from a package never means deletion from a tenant.
func mergeManagementPackageSnapshot(current, incoming *doctype.ConfigSnapshot, authority packageAuthority) (*doctype.ConfigSnapshot, error) {
	if current == nil || incoming == nil {
		return nil, fmt.Errorf("package activation requires current and incoming config snapshots")
	}
	if err := validateOwnedResourceReplacements(current, incoming); err != nil {
		return nil, err
	}
	merged := *current
	merged.DocTypes = mergeSnapshotItems(current.DocTypes, incoming.DocTypes, func(v *doctype.DocType) string { return v.Name })
	merged.Roles = mergeSnapshotItems(current.Roles, incoming.Roles, func(v *doctype.Role) string { return v.Name })
	merged.Permissions = mergeSnapshotItems(current.Permissions, incoming.Permissions, func(v *doctype.Permission) string { return v.Doctype + "\x00" + v.Role })
	merged.WorkflowDefinitions = mergeWorkflowDefinitions(current.WorkflowDefinitions, incoming.WorkflowDefinitions)
	merged.Views = mergeSnapshotItems(current.Views, incoming.Views, func(v *doctype.View) string { return v.Name })
	merged.Scripts = mergeSnapshotItems(current.Scripts, incoming.Scripts, func(v *doctype.ScriptSnapshot) string { return v.Name })
	modules, err := mergeModuleDescriptors(current.Modules, incoming.Modules, authority)
	if err != nil {
		return nil, err
	}
	merged.Modules = modules
	if err := mergeRawPackageItems(&merged, current, incoming); err != nil {
		return nil, err
	}
	merged.ScheduledJobs = mergeScheduledJobs(current.ScheduledJobs, incoming.ScheduledJobs)
	if err := validateMergedViewRoutes(merged.Views); err != nil {
		return nil, err
	}
	return &merged, nil
}

// mergeRawPackageItems merges the raw-encoded config (commands, queries, print
// formats) and then drops what replaced modules owned that the package no
// longer ships. Queries are parsed against the merged record types.
func mergeRawPackageItems(merged *doctype.ConfigSnapshot, current, incoming *doctype.ConfigSnapshot) error {
	registry := doctype.NewRegistry()
	for _, dt := range merged.DocTypes {
		registry.Register(dt)
	}
	var err error
	if merged.Commands, err = mergeRawSnapshotItems(current.Commands, incoming.Commands, func(raw json.RawMessage) (string, error) {
		def, err := kernel.ParseCommandResourceJSON(raw)
		if err != nil {
			return "", err
		}
		return def.Namespace + "." + def.Name, nil
	}); err != nil {
		return fmt.Errorf("merge package commands: %w", err)
	}
	if merged.Queries, err = mergeRawSnapshotItems(current.Queries, incoming.Queries, func(raw json.RawMessage) (string, error) {
		def, err := query.Parse(raw, registry)
		if err != nil {
			return "", err
		}
		return def.ModuleOwnerID + "/" + def.Name, nil
	}); err != nil {
		return fmt.Errorf("merge package queries: %w", err)
	}
	if merged.PrintFormats, err = mergeRawSnapshotItems(current.PrintFormats, incoming.PrintFormats, func(raw json.RawMessage) (string, error) {
		def, err := print.Decode(raw)
		if err != nil {
			return "", err
		}
		return def.Name, nil
	}); err != nil {
		return fmt.Errorf("merge package print formats: %w", err)
	}
	if merged.Metrics, err = mergeRawSnapshotItems(current.Metrics, incoming.Metrics, definitionName); err != nil {
		return fmt.Errorf("merge package metrics: %w", err)
	}
	if merged.Dashboards, err = mergeRawSnapshotItems(current.Dashboards, incoming.Dashboards, definitionName); err != nil {
		return fmt.Errorf("merge package dashboards: %w", err)
	}
	return dropReplacedModuleItems(merged, incoming, replacedModuleIDs(current.Modules, incoming.Modules), registry)
}

// dropReplacedModuleItems removes the commands, queries and record types that a
// replaced module owned in the tenant and the newer package no longer ships.
// Tenant data in a dropped record type stays in its table, as with any package
// that omits an entity.
func dropReplacedModuleItems(merged *doctype.ConfigSnapshot, incoming *doctype.ConfigSnapshot, replaced map[contract.ModuleID]bool, registry *doctype.Registry) error {
	if len(replaced) == 0 {
		return nil
	}
	shippedDocTypes := make(map[string]bool, len(incoming.DocTypes))
	for _, dt := range incoming.DocTypes {
		if dt != nil {
			shippedDocTypes[dt.Name] = true
		}
	}
	merged.DocTypes = slices.DeleteFunc(merged.DocTypes, func(dt *doctype.DocType) bool {
		return dt != nil && replaced[contract.ModuleID(dt.ModuleOwnerID)] && !shippedDocTypes[dt.Name]
	})
	shippedCommands, err := rawIdentities(incoming.Commands, commandIdentity)
	if err != nil {
		return err
	}
	if merged.Commands, err = dropRawItems(merged.Commands, replaced, shippedCommands, commandIdentity); err != nil {
		return err
	}
	queryIdentity := func(raw json.RawMessage) (string, contract.ModuleID, error) { return queryIdentityIn(raw, registry) }
	shippedQueries, err := rawIdentities(incoming.Queries, queryIdentity)
	if err != nil {
		return err
	}
	if merged.Queries, err = dropRawItems(merged.Queries, replaced, shippedQueries, queryIdentity); err != nil {
		return err
	}
	return nil
}

// replacedModuleIDs lists the installed modules that the package replaces with a different version.
func replacedModuleIDs(current, incoming []contract.ModuleDescriptor) map[contract.ModuleID]bool {
	installed := make(map[contract.ModuleID]string, len(current))
	for _, module := range current {
		installed[module.ID] = module.Version
	}
	replaced := make(map[contract.ModuleID]bool)
	for _, module := range incoming {
		if version, ok := installed[module.ID]; ok && version != module.Version {
			replaced[module.ID] = true
		}
	}
	return replaced
}

// rawIdentity names a raw config entry and the module that owns it.
type rawIdentity func(json.RawMessage) (string, contract.ModuleID, error)

func commandIdentity(raw json.RawMessage) (string, contract.ModuleID, error) {
	def, err := kernel.ParseCommandResourceJSON(raw)
	if err != nil {
		return "", "", err
	}
	return def.Namespace + "." + def.Name, def.ModuleOwnerID, nil
}

func queryIdentityIn(raw json.RawMessage, registry *doctype.Registry) (string, contract.ModuleID, error) {
	def, err := query.Parse(raw, registry)
	if err != nil {
		return "", "", err
	}
	return def.ModuleOwnerID + "/" + def.Name, contract.ModuleID(def.ModuleOwnerID), nil
}

func rawIdentities(items []json.RawMessage, identify rawIdentity) (map[string]bool, error) {
	keys := make(map[string]bool, len(items))
	for _, raw := range items {
		key, _, err := identify(raw)
		if err != nil {
			return nil, err
		}
		keys[key] = true
	}
	return keys, nil
}

// dropRawItems keeps every entry except those a replaced module owns that the package does not ship.
func dropRawItems(items []json.RawMessage, replaced map[contract.ModuleID]bool, shipped map[string]bool, identify rawIdentity) ([]json.RawMessage, error) {
	kept := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		key, owner, err := identify(raw)
		if err != nil {
			return nil, err
		}
		if !(replaced[owner] && !shipped[key]) {
			kept = append(kept, raw)
		}
	}
	return kept, nil
}

func validateOwnedResourceReplacements(current, incoming *doctype.ConfigSnapshot) error {
	owners := make(map[string]contract.ModuleID)
	for _, module := range current.Modules {
		for _, resource := range module.Resources {
			owners[resource.Ref.Namespace+"\x00"+resource.Ref.Name] = module.ID
		}
	}
	incomingOwners := make(map[string]contract.ModuleID)
	for _, module := range incoming.Modules {
		for _, resource := range module.Resources {
			incomingOwners[resource.Ref.Namespace+"\x00"+resource.Ref.Name] = module.ID
		}
	}
	for key, owner := range owners {
		namespace, name, _ := strings.Cut(key, "\x00")
		if !packageContainsResource(incoming, namespace, name) {
			continue
		}
		if replacementOwner := incomingOwners[key]; replacementOwner != owner {
			return fmt.Errorf("package resource %s/%s is owned by module %q and cannot be replaced without that module's update", namespace, name, owner)
		}
	}
	return nil
}

// packageResourceLookups finds a named resource of one kind (the namespace) in a package snapshot.
var packageResourceLookups = map[string]func(*doctype.ConfigSnapshot, string) bool{
	"tenant": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.DocTypes, func(v *doctype.DocType) bool { return v != nil && v.Name == name })
	},
	"role": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.Roles, func(v *doctype.Role) bool { return v != nil && v.Name == name })
	},
	"permission": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.Permissions, func(v *doctype.Permission) bool { return v != nil && v.Doctype+"/"+v.Role == name })
	},
	"workflow": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.WorkflowDefinitions, func(v contract.WorkflowDefinition) bool { return string(v.ID) == name })
	},
	"view": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.Views, func(v *doctype.View) bool { return v != nil && v.Name == name })
	},
	"script": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.Scripts, func(v *doctype.ScriptSnapshot) bool { return v != nil && v.Name == name })
	},
	"job": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.ScheduledJobs, func(v contract.ScheduledJobDefinition) bool { return v.Name == name })
	},
	"query": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.Modules, func(m contract.ModuleDescriptor) bool {
			return containsMatch(m.Queries, func(v contract.PublishedQuery) bool { return v.Name == name })
		})
	},
	"projection": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.Modules, func(m contract.ModuleDescriptor) bool {
			return containsMatch(m.Projections, func(v contract.ProjectionDefinition) bool { return v.ID == name })
		})
	},
	"command": func(s *doctype.ConfigSnapshot, name string) bool {
		return containsMatch(s.Modules, func(m contract.ModuleDescriptor) bool {
			return containsMatch(m.Provides, func(offer contract.CapabilityOffer) bool {
				return containsMatch(offer.Operations, func(op contract.OperationSpec) bool {
					return string(offer.Name)+"."+op.Name == name
				})
			})
		})
	},
}

// packageContainsResource reports whether the package snapshot carries a resource of the kind and name.
func packageContainsResource(snapshot *doctype.ConfigSnapshot, namespace, name string) bool {
	lookup, ok := packageResourceLookups[namespace]
	return ok && lookup(snapshot, name)
}

// containsMatch reports whether any item satisfies match.
func containsMatch[T any](items []T, match func(T) bool) bool {
	for _, item := range items {
		if match(item) {
			return true
		}
	}
	return false
}

// packageAuthority is what a package activation may do to modules the tenant
// already has: it names the package being activated and the package that
// installed each current module. The zero value authorizes no replacement.
type packageAuthority struct {
	packageID   string
	installedBy map[contract.ModuleID]string
}

// newPackageAuthority reads which installed package owns each module through
// the artifact digest that recorded the module's installation.
func newPackageAuthority(release contract.InstalledPackage, installed contract.ModuleSnapshot) packageAuthority {
	packageByDigest := make(map[string]string, len(installed.Packages))
	for _, pkg := range installed.Packages {
		packageByDigest[pkg.Digest] = pkg.PackageID
	}
	installedBy := make(map[contract.ModuleID]string, len(installed.Installations))
	for _, installation := range installed.Installations {
		installedBy[installation.ModuleID] = packageByDigest[installation.ArtifactDigest]
	}
	return packageAuthority{packageID: release.PackageID, installedBy: installedBy}
}

// moduleVersion is a module version of one to three dot-separated decimal
// numbers, such as "1", "1.2" or "1.2.3". Pack versions reach it as semver.
type moduleVersion string

func (v moduleVersion) parts() ([3]uint64, error) {
	var out [3]uint64
	pieces := strings.Split(string(v), ".")
	if len(pieces) > len(out) {
		return out, fmt.Errorf("module version %q has more than three components", v)
	}
	for i, piece := range pieces {
		n, err := strconv.ParseUint(piece, 10, 64)
		if err != nil {
			return out, fmt.Errorf("module version %q is not dotted decimal numbers", v)
		}
		out[i] = n
	}
	return out, nil
}

// newerThan reports whether v is strictly greater than installed.
func (v moduleVersion) newerThan(installed moduleVersion) (bool, error) {
	next, err := v.parts()
	if err != nil {
		return false, err
	}
	current, err := installed.parts()
	if err != nil {
		return false, err
	}
	return slices.Compare(next[:], current[:]) > 0, nil
}

// authorizeModuleReplacement decides whether an incoming module may replace an
// installed one. The same version must be identical. A different version must
// be strictly newer and come from the package that installed the module.
func authorizeModuleReplacement(authority packageAuthority, installed, next contract.ModuleDescriptor) error {
	if installed.Version == next.Version {
		previous, _ := json.Marshal(installed)
		incoming, _ := json.Marshal(next)
		if string(previous) != string(incoming) {
			return fmt.Errorf("module %q content cannot change without an authorized version update", next.ID)
		}
		return nil
	}
	newer, err := moduleVersion(next.Version).newerThan(moduleVersion(installed.Version))
	if err != nil {
		return fmt.Errorf("module %q version %q is not a comparable version: %w", next.ID, next.Version, err)
	}
	if !newer {
		return fmt.Errorf("module %q version %q does not replace installed version %q: downgrades are refused", next.ID, next.Version, installed.Version)
	}
	if authority.packageID == "" {
		return fmt.Errorf("module %q update requires explicit authorized replacement", next.ID)
	}
	if owner := authority.installedBy[next.ID]; owner != authority.packageID {
		return fmt.Errorf("module %q is installed by package %q, so package %q cannot replace it", next.ID, owner, authority.packageID)
	}
	return nil
}

func mergeModuleDescriptors(current, incoming []contract.ModuleDescriptor, authority packageAuthority) ([]contract.ModuleDescriptor, error) {
	byID := make(map[contract.ModuleID]contract.ModuleDescriptor, len(current)+len(incoming))
	owners := make(map[string]contract.ModuleID)
	for _, module := range current {
		byID[module.ID] = module
		for _, resource := range module.Resources {
			owners[resource.Ref.Namespace+"\x00"+resource.Ref.Name] = module.ID
		}
	}
	for _, module := range incoming {
		if existing, ok := byID[module.ID]; ok {
			if err := authorizeModuleReplacement(authority, existing, module); err != nil {
				return nil, err
			}
		}
		for _, resource := range module.Resources {
			key := resource.Ref.Namespace + "\x00" + resource.Ref.Name
			if owner, ok := owners[key]; ok && owner != module.ID {
				return nil, fmt.Errorf("resource %q is already owned by module %q", resource.Ref.String(), owner)
			}
			owners[key] = module.ID
		}
		byID[module.ID] = module
	}
	modules := make([]contract.ModuleDescriptor, 0, len(byID))
	for _, module := range byID {
		modules = append(modules, module)
	}
	if err := contract.ValidateModuleSet(modules, nil); err != nil {
		return nil, err
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].ID < modules[j].ID })
	return modules, nil
}

func mergeWorkflowDefinitions(current, incoming []contract.WorkflowDefinition) []contract.WorkflowDefinition {
	definitions := make(map[contract.DefinitionID]contract.WorkflowDefinition, len(current)+len(incoming))
	for _, definition := range current {
		definitions[definition.ID] = definition
	}
	for _, definition := range incoming {
		definitions[definition.ID] = definition
	}
	merged := make([]contract.WorkflowDefinition, 0, len(definitions))
	for _, definition := range definitions {
		merged = append(merged, definition)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	return merged
}

func mergeScheduledJobs(current, incoming []contract.ScheduledJobDefinition) []contract.ScheduledJobDefinition {
	jobs := make(map[string]contract.ScheduledJobDefinition, len(current)+len(incoming))
	for _, job := range current {
		jobs[job.Name] = job
	}
	for _, job := range incoming {
		jobs[job.Name] = job
	}
	merged := make([]contract.ScheduledJobDefinition, 0, len(jobs))
	for _, job := range jobs {
		merged = append(merged, job)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Name < merged[j].Name })
	return merged
}

func mergeSnapshotItems[T any](current, incoming []*T, key func(*T) string) []*T {
	items := make([]*T, 0, len(current)+len(incoming))
	indices := make(map[string]int, len(current)+len(incoming))
	for _, item := range current {
		if item == nil {
			continue
		}
		name := key(item)
		if idx, ok := indices[name]; ok {
			items[idx] = item
			continue
		}
		indices[name] = len(items)
		items = append(items, item)
	}
	for _, item := range incoming {
		if item == nil {
			continue
		}
		name := key(item)
		if idx, ok := indices[name]; ok {
			items[idx] = item
			continue
		}
		indices[name] = len(items)
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return key(items[i]) < key(items[j]) })
	return items
}

func validateMergedViewRoutes(views []*doctype.View) error {
	owners := make(map[string]string, len(views))
	for _, view := range views {
		if view == nil || strings.TrimSpace(view.Route) == "" {
			continue
		}
		route := strings.ToLower(strings.TrimSpace(view.Route))
		if previous, ok := owners[route]; ok && previous != view.Name {
			return fmt.Errorf("package view route %q conflicts with existing view %q", view.Route, previous)
		}
		owners[route] = view.Name
	}
	return nil
}

// mergeRawSnapshotItems merges raw snapshot entries by decoded key: an incoming
// entry replaces the current one with the same key, new ones are appended. An
// entry that cannot be decoded fails the merge.
func mergeRawSnapshotItems(current, incoming []json.RawMessage, key func(json.RawMessage) (string, error)) ([]json.RawMessage, error) {
	items := make([]json.RawMessage, 0, len(current)+len(incoming))
	indices := make(map[string]int, len(current)+len(incoming))
	for _, group := range [][]json.RawMessage{current, incoming} {
		for _, raw := range group {
			name, err := key(raw)
			if err != nil {
				return nil, err
			}
			if idx, ok := indices[name]; ok {
				items[idx] = raw
				continue
			}
			indices[name] = len(items)
			items = append(items, raw)
		}
	}
	return items, nil
}

// definitionName is the key of a metric or dashboard definition in a config snapshot.
func definitionName(raw json.RawMessage) (string, error) {
	var m struct {
		Name string `json:"name"`
	}
	err := json.Unmarshal(raw, &m)
	return m.Name, err
}
