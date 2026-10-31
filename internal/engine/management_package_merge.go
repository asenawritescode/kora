package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// mergeManagementPackageSnapshot preserves tenant-owned configuration and
// overlays only entities named by the package. Package import is additive and
// upserting; omission from a package never means deletion from a tenant.
func mergeManagementPackageSnapshot(current, incoming *doctype.ConfigSnapshot) (*doctype.ConfigSnapshot, error) {
	if current == nil || incoming == nil {
		return nil, fmt.Errorf("package activation requires current and incoming config snapshots")
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
	if err := validateMergedViewRoutes(merged.Views); err != nil {
		return nil, err
	}
	return &merged, nil
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
