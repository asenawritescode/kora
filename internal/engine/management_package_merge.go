package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
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
func mergeManagementPackageSnapshot(current, incoming *doctype.ConfigSnapshot) (*doctype.ConfigSnapshot, error) {
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
	merged.AnalyticsMetrics = mergeSnapshotItems(current.AnalyticsMetrics, incoming.AnalyticsMetrics, func(v *doctype.AnalyticsMetricConfig) string { return v.Name })
	merged.Scripts = mergeSnapshotItems(current.Scripts, incoming.Scripts, func(v *doctype.ScriptSnapshot) string { return v.Name })
	merged.Reports = append([]json.RawMessage(nil), current.Reports...)
	merged.ScheduledJobs = mergeScheduledJobs(current.ScheduledJobs, incoming.ScheduledJobs)
	modules, err := mergeModuleDescriptors(current.Modules, incoming.Modules)
	if err != nil {
		return nil, err
	}
	merged.Modules = modules
	if err := validateMergedViewRoutes(merged.Views); err != nil {
		return nil, err
	}
	return &merged, nil
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

func packageContainsResource(snapshot *doctype.ConfigSnapshot, namespace, name string) bool {
	switch namespace {
	case "tenant":
		for _, value := range snapshot.DocTypes {
			if value != nil && value.Name == name {
				return true
			}
		}
	case "role":
		for _, value := range snapshot.Roles {
			if value != nil && value.Name == name {
				return true
			}
		}
	case "permission":
		for _, value := range snapshot.Permissions {
			if value != nil && value.Doctype+"/"+value.Role == name {
				return true
			}
		}
	case "workflow":
		for _, value := range snapshot.WorkflowDefinitions {
			if string(value.ID) == name {
				return true
			}
		}
	case "view":
		for _, value := range snapshot.Views {
			if value != nil && value.Name == name {
				return true
			}
		}
	case "metric":
		for _, value := range snapshot.AnalyticsMetrics {
			if value != nil && value.Name == name {
				return true
			}
		}
	case "script":
		for _, value := range snapshot.Scripts {
			if value != nil && value.Name == name {
				return true
			}
		}
	case "job":
		for _, value := range snapshot.ScheduledJobs {
			if value.Name == name {
				return true
			}
		}
	case "query":
		for _, module := range snapshot.Modules {
			for _, value := range module.Queries {
				if value.Name == name {
					return true
				}
			}
		}
	case "projection":
		for _, module := range snapshot.Modules {
			for _, value := range module.Projections {
				if value.ID == name {
					return true
				}
			}
		}
	case "command":
		for _, module := range snapshot.Modules {
			for _, offer := range module.Provides {
				for _, operation := range offer.Operations {
					if string(offer.Name)+"."+operation.Name == name {
						return true
					}
				}
			}
		}
	case "report":
		for _, value := range snapshot.Reports {
			var report struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(value, &report) == nil && report.Name == name {
				return true
			}
		}
	}
	return false
}

func mergeModuleDescriptors(current, incoming []contract.ModuleDescriptor) ([]contract.ModuleDescriptor, error) {
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
			if existing.Version != module.Version {
				return nil, fmt.Errorf("module %q update requires explicit authorized replacement", module.ID)
			}
			previous, _ := json.Marshal(existing)
			next, _ := json.Marshal(module)
			if string(previous) != string(next) {
				return nil, fmt.Errorf("module %q content cannot change without an authorized version update", module.ID)
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
