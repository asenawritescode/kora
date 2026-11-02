package doctype

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ReferenceFinding describes a configuration reference that cannot be safely
// resolved against the candidate snapshot and the site's existing registry.
type ReferenceFinding struct {
	Code       string `json:"code"`
	Severity   string `json:"severity"`
	Path       string `json:"path"`
	TargetKind string `json:"target_kind"`
	Target     string `json:"target"`
	Message    string `json:"message"`
}

// ReferenceValidationError is returned by configuration persistence when a
// candidate snapshot contains unresolved references.
type ReferenceValidationError struct{ Findings []ReferenceFinding }

func (e *ReferenceValidationError) Error() string {
	if e == nil || len(e.Findings) == 0 {
		return "configuration references are invalid"
	}
	first := e.Findings[0]
	return fmt.Sprintf("configuration reference validation failed: %s at %s", first.Code, first.Path)
}

// ValidateSnapshotReferences checks references in a candidate snapshot. The
// candidate definitions shadow live definitions, which permits a package to
// update a DocType and its consumers in one atomic snapshot.
func ValidateSnapshotReferences(candidate *ConfigSnapshot, live *Registry) []ReferenceFinding {
	if candidate == nil {
		return []ReferenceFinding{{Code: "snapshot.missing", Severity: "error", Path: "$", TargetKind: "snapshot", Message: "candidate snapshot is required"}}
	}

	defs := map[string]*DocType{}
	for _, dt := range candidate.DocTypes {
		if dt != nil && strings.TrimSpace(dt.Name) != "" {
			defs[referenceKey(dt.Name)] = dt
		}
	}
	getDoc := func(name string) *DocType {
		if dt := defs[referenceKey(name)]; dt != nil {
			return dt
		}
		if live != nil {
			return live.Get(strings.TrimSpace(name))
		}
		return nil
	}
	pageRegistry := NewRegistry()
	if live != nil {
		pageRegistry.LoadFull(live.All(), nil, nil)
		if live.Views != nil {
			pageRegistry.Views.LoadFromDB(live.Views.All())
		}
	}
	for _, dt := range candidate.DocTypes {
		if dt != nil {
			pageRegistry.Register(dt)
		}
	}

	var findings []ReferenceFinding
	add := func(code, path, kind, target, message string) {
		findings = append(findings, ReferenceFinding{Code: code, Severity: "error", Path: path, TargetKind: kind, Target: target, Message: message})
	}
	checkDoc := func(path, name string) *DocType {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil
		}
		dt := getDoc(name)
		if dt == nil {
			add("reference.doctype_missing", path, "doctype", name, fmt.Sprintf("DocType %q does not exist in the candidate snapshot or site registry", name))
		}
		return dt
	}
	checkField := func(path string, dt *DocType, field string) {
		field = strings.TrimSpace(field)
		if dt == nil || field == "" || field == "*" || strings.HasPrefix(field, "literal:") || strings.ContainsAny(field, "$`{}() ") || isReferenceSystemField(field) {
			return
		}
		for i := range dt.Fields {
			if strings.EqualFold(dt.Fields[i].Fieldname, field) {
				return
			}
		}
		add("reference.field_missing", path, "field", dt.Name+"."+field, fmt.Sprintf("Field %q does not exist on DocType %q", field, dt.Name))
	}

	for _, dt := range candidate.DocTypes {
		if dt == nil {
			continue
		}
		for fi, field := range dt.Fields {
			path := fmt.Sprintf("doctypes[%s].fields[%d]", dt.Name, fi)
			if field.ReferenceRule != nil {
				if err := field.ReferenceRule.Validate(); err != nil {
					add("reference.rule_invalid", path+".reference_rule", "reference_rule", field.ReferenceRule.Kind, err.Error())
				}
				if field.ReferenceRule.Kind == "local_owned" && field.Fieldtype != "Link" {
					add("reference.local_rule_requires_link", path+".reference_rule", "field", field.Fieldname, "local_owned reference rule requires a Link field")
				}
				if field.ReferenceRule.Kind == "published_business_ref" && field.Fieldtype == "Link" {
					add("reference.published_rule_cannot_use_link", path+".reference_rule", "field", field.Fieldname, "published business references use a provider-qualified identity field")
				}
			}
			switch strings.ToLower(strings.TrimSpace(field.Fieldtype)) {
			case "link", "table":
				target := checkDoc(path+".options", field.Options)
				if target != nil && strings.EqualFold(field.Fieldtype, "Table") && !target.IsChildTable {
					add("reference.child_doctype_incompatible", path+".options", "doctype", target.Name, fmt.Sprintf("Table field %q must reference a child DocType", field.Fieldname))
				}
			}
			if strings.TrimSpace(field.LinkedField) != "" {
				linkName, sourceName, ok := strings.Cut(field.LinkedField, ".")
				if !ok || strings.TrimSpace(linkName) == "" || strings.TrimSpace(sourceName) == "" {
					add("reference.linked_field_invalid", path+".linked_field", "field", field.LinkedField, "linked_field must use the form link_field.source_field")
					continue
				}
				linkField := dt.GetField(linkName)
				if linkField == nil || !strings.EqualFold(strings.TrimSpace(linkField.Fieldtype), "Link") {
					add("reference.linked_field_incompatible", path+".linked_field", "field", field.LinkedField, fmt.Sprintf("linked_field %q must use a Link field on DocType %q", field.LinkedField, dt.Name))
					continue
				}
				linkedDT := checkDoc(path+".linked_field", linkField.Options)
				checkField(path+".linked_field", linkedDT, sourceName)
			}
		}
	}

	seenRoutes := map[string]string{}
	for _, view := range candidate.Views {
		if view == nil {
			continue
		}
		viewPath := "views[" + view.Name + "]"
		route := normalizeViewRoute(view.Route)
		if prior := seenRoutes[route]; prior != "" {
			add("reference.route_ambiguous", viewPath+".route", "route", route, fmt.Sprintf("Route %q is also registered by view %q", route, prior))
		} else if route != "" {
			seenRoutes[route] = view.Name
		}
		// A DocType route is the kebab-case form used by the workspace router.
		for _, dt := range append(candidateDocTypes(candidate), liveDocTypes(live)...) {
			if dt != nil && route != "" && strings.Trim(strings.ToLower(route), "/") == normalizeRouteSlug(dt.Name) {
				add("reference.route_ambiguous", viewPath+".route", "route", route, fmt.Sprintf("Page route %q conflicts with DocType route for %q", route, dt.Name))
				break
			}
		}
		viewDT := checkDoc(viewPath+".source_doctype", view.SourceDocType)
		if view.PageManifest != nil {
			for _, finding := range ValidatePageManifestReferences(view.PageManifest, pageRegistry) {
				finding.Path = viewPath + ".page_manifest." + finding.Path
				findings = append(findings, finding)
			}
		}
		if view.Task != nil {
			for i, name := range append(append([]string{}, view.Task.Reads...), view.Task.Writes...) {
				checkDoc(fmt.Sprintf("%s.task.records[%d]", viewPath, i), name)
			}
		}
		if view.PageManifest == nil {
			validateViewReferences(view, viewPath, viewDT, checkDoc, checkField, add)
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Path == findings[j].Path {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Path < findings[j].Path
	})
	return findings
}

// ValidatePageManifestReferences validates a manifest before it is persisted.
func ValidatePageManifestReferences(manifest *PageManifest, live *Registry) []ReferenceFinding {
	if manifest == nil {
		return []ReferenceFinding{{Code: "manifest.missing", Severity: "error", Path: "$", TargetKind: "page", Message: "page manifest is required"}}
	}
	snapshot := &ConfigSnapshot{}
	if live != nil && live.Views != nil {
		for _, current := range live.Views.All() {
			if current != nil && !strings.EqualFold(current.Name, manifest.Metadata.Name) {
				copy := *current
				copy.PageManifest = nil
				snapshot.Views = append(snapshot.Views, &copy)
			}
		}
	}
	manifestView := manifest.ToView()
	manifestView.PageManifest = nil
	snapshot.Views = append(snapshot.Views, manifestView)
	findings := ValidateSnapshotReferences(snapshot, live)
	defs := map[string]*DocType{}
	if live != nil {
		for _, dt := range live.All() {
			if dt != nil {
				defs[referenceKey(dt.Name)] = dt
			}
		}
	}
	getDoc := func(name string) *DocType { return defs[referenceKey(name)] }
	checkDoc := func(path, name string) *DocType {
		if strings.TrimSpace(name) == "" {
			return nil
		}
		dt := getDoc(name)
		if dt == nil {
			findings = append(findings, ReferenceFinding{Code: "reference.doctype_missing", Severity: "error", Path: path, TargetKind: "doctype", Target: name, Message: fmt.Sprintf("DocType %q does not exist in the site registry", name)})
		}
		return dt
	}
	checkField := func(path string, dt *DocType, field string) {
		if dt == nil || field == "" || field == "*" || strings.HasPrefix(field, "literal:") || strings.ContainsAny(field, "$`{}() ") || isReferenceSystemField(field) {
			return
		}
		for _, f := range dt.Fields {
			if strings.EqualFold(f.Fieldname, field) {
				return
			}
		}
		findings = append(findings, ReferenceFinding{Code: "reference.field_missing", Severity: "error", Path: path, TargetKind: "field", Target: dt.Name + "." + field, Message: fmt.Sprintf("Field %q does not exist on DocType %q", field, dt.Name)})
	}
	resourceIDs := map[string]bool{}
	seenResourceIDs := map[string]bool{}
	resourceDocs := map[string]*DocType{}
	for _, resource := range manifest.Spec.Resources {
		resourceIDs[resource.ID] = true
	}
	addPageFinding := func(code, path, kind, target, message string) {
		findings = append(findings, ReferenceFinding{Code: code, Severity: "error", Path: path, TargetKind: kind, Target: target, Message: message})
	}
	for i, resource := range manifest.Spec.Resources {
		path := fmt.Sprintf("spec.resources[%d]", i)
		if resource.ID == "" || seenResourceIDs[resource.ID] {
			findings = append(findings, ReferenceFinding{Code: "reference.resource_id_duplicate", Severity: "error", Path: path + ".id", TargetKind: "resource", Target: resource.ID, Message: "resource IDs must be non-empty and unique"})
		}
		seenResourceIDs[resource.ID] = true
		if resource.Query != "document.list" && resource.Query != "document.get" {
			findings = append(findings, ReferenceFinding{Code: "reference.query_unsupported", Severity: "error", Path: path + ".query", TargetKind: "query", Target: resource.Query, Message: fmt.Sprintf("query type %q is not supported by reference validation", resource.Query)})
		}
		doctypeName, _ := resource.Params["doctype"].(string)
		dt := checkDoc(path+".params.doctype", doctypeName)
		resourceDocs[resource.ID] = dt
		if raw, ok := resource.Params["fields"].([]any); ok {
			for fi, value := range raw {
				if field, ok := value.(string); ok {
					checkField(fmt.Sprintf("%s.params.fields[%d]", path, fi), dt, field)
				}
			}
		}
		if filters, ok := resource.Params["filters"].([]any); ok {
			for fi, rawFilter := range filters {
				if filter, ok := rawFilter.(map[string]any); ok {
					field, _ := filter["field"].(string)
					checkField(fmt.Sprintf("%s.params.filters[%d].field", path, fi), dt, field)
				}
			}
		}
		for j, dependency := range resource.DependsOn {
			if !resourceIDs[dependency] {
				findings = append(findings, ReferenceFinding{Code: "reference.resource_missing", Severity: "error", Path: fmt.Sprintf("%s.depends_on[%d]", path, j), TargetKind: "resource", Target: dependency, Message: fmt.Sprintf("resource dependency %q is not declared", dependency)})
			}
		}
	}
	componentIDs := map[string]bool{}
	usedActionIDs := map[string]bool{}
	actionCommandByID := make(map[string]string, len(manifest.Spec.Actions))
	for _, action := range manifest.Spec.Actions {
		actionCommandByID[action.ID] = action.Command
	}
	var visitComponents func([]PageComponent)
	visitComponents = func(children []PageComponent) {
		for _, child := range children {
			path := "spec.layout.children[" + child.ID + "]"
			if child.ID == "" || componentIDs[child.ID] {
				findings = append(findings, ReferenceFinding{Code: "reference.component_id_duplicate", Severity: "error", Path: path + ".id", TargetKind: "component", Target: child.ID, Message: "component IDs must be non-empty and unique"})
			}
			componentIDs[child.ID] = true
			componentContract, knownComponent := PageComponentContractFor(child.Component)
			if !knownComponent {
				findings = append(findings, ReferenceFinding{Code: "reference.component_missing", Severity: "error", Path: path + ".component", TargetKind: "component", Target: child.Component, Message: fmt.Sprintf("component %q is not registered by this Kora UI runtime", child.Component)})
			} else if child.Version != componentContract.Version {
				findings = append(findings, ReferenceFinding{Code: "reference.component_version_unsupported", Severity: "error", Path: path + ".version", TargetKind: "component_version", Target: fmt.Sprintf("%s@%d", child.Component, child.Version), Message: fmt.Sprintf("component %q requires version %d", child.Component, componentContract.Version)})
			}
			for _, actionID := range child.Actions {
				usedActionIDs[actionID] = true
			}
			if knownComponent {
				for _, requiredCommand := range componentContract.RequiredActions {
					found := false
					for _, actionID := range child.Actions {
						found = found || actionCommandByID[actionID] == requiredCommand
					}
					if !found {
						findings = append(findings, ReferenceFinding{Code: "reference.component_action_missing", Severity: "error", Path: path + ".actions", TargetKind: "action_command", Target: requiredCommand, Message: fmt.Sprintf("component %q requires an action with command %q", child.Component, requiredCommand)})
					}
				}
			}
			dataRef := strings.TrimSpace(child.Data)
			var boundDataDoc *DocType
			if dataRef != "" {
				resourceID := strings.TrimSuffix(dataRef, ".data")
				boundDataDoc = resourceDocs[resourceID]
				if !resourceIDs[resourceID] {
					findings = append(findings, ReferenceFinding{Code: "reference.resource_missing", Severity: "error", Path: path + ".data", TargetKind: "resource", Target: resourceID, Message: fmt.Sprintf("resource %q is not declared by this page", resourceID)})
				}
			} else if knownComponent && componentContract.RequiresData {
				findings = append(findings, ReferenceFinding{Code: "reference.component_resource_missing", Severity: "error", Path: path + ".data", TargetKind: "resource", Message: fmt.Sprintf("component %q requires a declared resource binding", child.Component)})
			}
			componentDoc := boundDataDoc
			if dtName, ok := child.Props["source_doctype"].(string); ok && strings.TrimSpace(dtName) != "" {
				dt := checkDoc(path+".props.source_doctype", dtName)
				componentDoc = dt
				if boundDataDoc != nil && !strings.EqualFold(boundDataDoc.Name, dtName) {
					findings = append(findings, ReferenceFinding{Code: "reference.component_resource_incompatible", Severity: "error", Path: path + ".data", TargetKind: "doctype", Target: boundDataDoc.Name, Message: fmt.Sprintf("component source DocType %q does not match resource DocType %q", dtName, boundDataDoc.Name)})
				}
			}
			componentResourceDocs := map[string]*DocType{}
			if knownComponent {
				for role, resourceContract := range componentContract.Resources {
					resourceID := strings.TrimSpace(child.ResourceBindings[role])
					if resourceID == "" && role == "products" {
						resourceID = strings.TrimSuffix(dataRef, ".data")
					}
					if resourceID == "" {
						if resourceContract.Required {
							addPageFinding("reference.component_resource_required", path+".resource_bindings."+role, "resource", role, fmt.Sprintf("component %q requires resource role %q", child.Component, role))
						}
						continue
					}
					if !resourceIDs[resourceID] {
						addPageFinding("reference.resource_missing", path+".resource_bindings."+role, "resource", resourceID, fmt.Sprintf("resource %q bound to role %q is not declared by this page", resourceID, role))
						continue
					}
					var resource *PageResource
					for resourceIndex := range manifest.Spec.Resources {
						if manifest.Spec.Resources[resourceIndex].ID == resourceID {
							resource = &manifest.Spec.Resources[resourceIndex]
							break
						}
					}
					if resource != nil && resourceContract.Query != "" && resource.Query != resourceContract.Query {
						addPageFinding("reference.component_resource_incompatible", path+".resource_bindings."+role, "resource", resourceID, fmt.Sprintf("component resource role %q requires query %q", role, resourceContract.Query))
					}
					componentResourceDocs[role] = resourceDocs[resourceID]
					if resourceContract.MustMatchData && dataRef != "" && resourceID != strings.TrimSuffix(dataRef, ".data") {
						addPageFinding("reference.component_resource_incompatible", path+".resource_bindings."+role, "resource", resourceID, fmt.Sprintf("component resource role %q must use its bound data resource %q", role, strings.TrimSuffix(dataRef, ".data")))
					}
				}
				for role, resourceID := range child.ResourceBindings {
					if _, declared := componentContract.Resources[role]; !declared {
						addPageFinding("reference.component_resource_unsupported", path+".resource_bindings."+role, "resource_role", role, fmt.Sprintf("component %q does not declare resource role %q", child.Component, role))
					}
					if strings.TrimSpace(resourceID) == "" {
						addPageFinding("reference.component_resource_required", path+".resource_bindings."+role, "resource", role, "resource binding must name a page resource ID")
					}
				}
				if _, exists := componentResourceDocs["products"]; !exists && componentDoc != nil {
					componentResourceDocs["products"] = componentDoc
				}
				validateComponentRelations(child, path, componentContract, componentResourceDocs, addPageFinding)
			}
			validateComponentContract(child, path, componentContract, knownComponent, componentDoc, componentResourceDocs, checkDoc, checkField, addPageFinding)
			if componentDoc != nil {
				for _, propName := range []string{"desktop_columns", "mobile_columns"} {
					if values, ok := child.Props[propName].([]any); ok {
						for fieldIndex, value := range values {
							if field, ok := value.(string); ok {
								checkField(fmt.Sprintf("%s.props.%s[%d]", path, propName, fieldIndex), componentDoc, field)
							}
						}
					}
				}
			}
			visitComponents(child.Children)
		}
	}
	visitComponents(manifest.Spec.Layout.Children)
	seenActions := map[string]bool{}
	for i, action := range manifest.Spec.Actions {
		if action.ID == "" {
			findings = append(findings, ReferenceFinding{Code: "reference.action_id_missing", Severity: "error", Path: fmt.Sprintf("spec.actions[%d].id", i), TargetKind: "action", Message: "action ID is required"})
		}
		if seenActions[action.ID] {
			findings = append(findings, ReferenceFinding{Code: "reference.action_id_duplicate", Severity: "error", Path: fmt.Sprintf("spec.actions[%d].id", i), TargetKind: "action", Target: action.ID, Message: "action IDs must be unique"})
		}
		seenActions[action.ID] = true
		contract, knownCommand := PageActionContractFor(strings.TrimSpace(action.Command))
		if !knownCommand {
			findings = append(findings, ReferenceFinding{Code: "reference.action_command_unsupported", Severity: "error", Path: fmt.Sprintf("spec.actions[%d].command", i), TargetKind: "action_command", Target: action.Command, Message: fmt.Sprintf("action command %q is not registered", action.Command)})
		}
		if !usedActionIDs[action.ID] {
			findings = append(findings, ReferenceFinding{Code: "reference.action_unbound", Severity: "error", Path: fmt.Sprintf("spec.actions[%d].id", i), TargetKind: "action", Target: action.ID, Message: "page action is not bound to a component"})
		}
		for j, resourceID := range action.Invalidate {
			if !resourceIDs[resourceID] {
				findings = append(findings, ReferenceFinding{Code: "reference.resource_missing", Severity: "error", Path: fmt.Sprintf("spec.actions[%d].invalidate[%d]", i, j), TargetKind: "resource", Target: resourceID, Message: fmt.Sprintf("action invalidates undeclared resource %q", resourceID)})
			}
		}
		if knownCommand {
			findings = append(findings, validatePageActionInput(action, i, contract, getDoc, checkDoc, checkField)...)
		}
	}
	return normalizeReferenceFindings(findings)
}

func validateComponentContract(component PageComponent, path string, contract PageComponentContract, known bool, source *DocType, resources map[string]*DocType, checkDoc func(string, string) *DocType, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	if !known {
		return
	}
	for name, value := range component.Props {
		definition, declared := contract.Props[name]
		if !declared {
			continue // extra props are retained for forward-compatible renderers
		}
		if !pageValueMatches(definition.Type, value) {
			add("reference.component_prop_type", path+".props."+name, "component_prop", name, fmt.Sprintf("component prop %q must have type %s", name, definition.Type))
			continue
		}
		if definition.Reference == "doctype" {
			if target, ok := value.(string); ok {
				checkDoc(path+".props."+name, target)
			}
		}
	}
	bindings, hasBindings := stringMap(component.Props["bindings"])
	if raw, exists := component.Props["bindings"]; exists && raw != nil && !hasBindings {
		add("reference.component_bindings_type", path+".props.bindings", "component_bindings", component.Component, "component bindings must be an object of string values")
		return
	}
	for key, value := range bindings {
		definition, declared := contract.Bindings[key]
		if !declared {
			add("reference.component_binding_unsupported", path+".props.bindings."+key, "component_binding", key, fmt.Sprintf("component %q does not declare binding %q", component.Component, key))
			continue
		}
		bindingSource := source
		if definition.ResourceRole != "" {
			bindingSource = resources[definition.ResourceRole]
		}
		if err := validateBindingValue(definition, value, bindingSource, checkField, path+".props.bindings."+key); err != "" {
			add("reference.component_binding_invalid", path+".props.bindings."+key, "component_binding", key, err)
		}
	}
	for key, definition := range contract.Bindings {
		if definition.Required && strings.TrimSpace(bindings[key]) == "" {
			add("reference.component_binding_required", path+".props.bindings."+key, "component_binding", key, fmt.Sprintf("component %q requires binding %q", component.Component, key))
			continue
		}
		if definition.RequiredWhenResourceRole != "" && resources[definition.RequiredWhenResourceRole] != nil && strings.TrimSpace(bindings[key]) == "" {
			add("reference.component_binding_required", path+".props.bindings."+key, "component_binding", key, fmt.Sprintf("component %q requires binding %q when resource role %q is configured", component.Component, key, definition.RequiredWhenResourceRole))
			continue
		}
		if definition.Default != "" {
			if _, configured := bindings[key]; !configured {
				bindingSource := source
				if definition.ResourceRole != "" {
					bindingSource = resources[definition.ResourceRole]
				}
				if err := validateBindingValue(definition, definition.Default, bindingSource, checkField, path+".props.bindings."+key); err != "" {
					add("reference.component_binding_invalid", path+".props.bindings."+key, "component_binding", key, err)
				}
			}
		}
	}
}

func validateComponentRelations(component PageComponent, path string, contract PageComponentContract, resources map[string]*DocType, add func(string, string, string, string, string)) {
	bindings, _ := stringMap(component.Props["bindings"])
	for _, relation := range contract.Relations {
		source := resources[relation.SourceResource]
		if source == nil {
			continue
		}
		fieldName := strings.TrimSpace(bindings[relation.FieldBinding])
		field := source.GetField(fieldName)
		if field == nil {
			continue
		}
		target := resources[relation.TargetResource]
		resourcePath := path + ".resource_bindings." + relation.TargetResource
		if strings.EqualFold(field.Fieldtype, "Link") && relation.RequiredForLinkType && target == nil {
			add("reference.component_resource_required", resourcePath, "resource", relation.TargetResource, fmt.Sprintf("field %q links to %q, so component %q must bind that DocType as resource role %q", field.Fieldname, field.Options, component.Component, relation.TargetResource))
			continue
		}
		if target == nil {
			continue
		}
		if !strings.EqualFold(field.Fieldtype, "Link") || !strings.EqualFold(strings.TrimSpace(field.Options), strings.TrimSpace(target.Name)) {
			add("reference.component_resource_incompatible", resourcePath, "doctype", target.Name, fmt.Sprintf("resource role %q must match the Link target of %s.%s", relation.TargetResource, source.Name, field.Fieldname))
		}
	}
}

func validateBindingValue(contract PageBindingContract, value string, source *DocType, checkField func(string, *DocType, string), path string) string {
	value = strings.TrimSpace(value)
	switch contract.Kind {
	case "field":
		if value == "" {
			if contract.Required {
				return "a DocType field binding is required"
			}
			return ""
		}
		if source == nil {
			return fmt.Sprintf("cannot resolve field %q because the component has no resolvable source DocType", value)
		}
		field := source.GetField(value)
		if field == nil {
			checkField(path, source, value)
			return ""
		}
		if !containsFold(contract.FieldTypes, "*") && !containsFold(contract.FieldTypes, field.Fieldtype) {
			return fmt.Sprintf("field %q has type %q; component requires one of %s", value, field.Fieldtype, strings.Join(contract.FieldTypes, ", "))
		}
	case "field_list":
		if value == "" {
			if contract.Required {
				return "at least one field is required"
			}
			return ""
		}
		for _, field := range strings.Split(value, ",") {
			field = strings.TrimSpace(field)
			if field != "" {
				checkField(path, source, field)
			}
		}
	case "enum":
		if value == "" {
			value = contract.Default
		}
		if value != "" && !containsFold(contract.AllowedValues, value) {
			return fmt.Sprintf("value %q is not allowed; choose one of %s", value, strings.Join(contract.AllowedValues, ", "))
		}
	case "literal":
		if strings.HasPrefix(value, "literal:") && len(value) == len("literal:") {
			return "literal binding must include a value after literal:"
		}
	case "boolean":
		if value != "true" && value != "false" {
			return "binding must be true or false"
		}
	case "integer":
		if _, err := strconv.Atoi(value); err != nil {
			return "binding must be an integer"
		}
	case "json":
		var decoded any
		if err := json.Unmarshal([]byte(value), &decoded); err != nil {
			return "binding must contain valid JSON"
		}
	default:
		return fmt.Sprintf("unknown binding contract kind %q", contract.Kind)
	}
	return ""
}

func validatePageActionInput(action PageAction, index int, contract PageActionContract, getDoc func(string) *DocType, checkDoc func(string, string) *DocType, checkField func(string, *DocType, string)) []ReferenceFinding {
	path := fmt.Sprintf("spec.actions[%d].input", index)
	input := action.Input
	var findings []ReferenceFinding
	add := func(code, fieldPath, kind, target, message string) {
		findings = append(findings, ReferenceFinding{Code: code, Severity: "error", Path: fieldPath, TargetKind: kind, Target: target, Message: message})
	}
	for name, definition := range contract.Inputs {
		value, present := input[name]
		if !present || value == nil || value == "" {
			if definition.Required {
				add("reference.action_input_missing", path+"."+name, "action_input", name, fmt.Sprintf("action %q requires input %q", action.Command, name))
			}
			continue
		}
		if !pageValueMatches(definition.Type, value) {
			add("reference.action_input_type", path+"."+name, "action_input", name, fmt.Sprintf("action input %q must have type %s", name, definition.Type))
			continue
		}
	}
	for name := range input {
		if _, declared := contract.Inputs[name]; !declared {
			add("reference.action_input_unsupported", path+"."+name, "action_input", name, fmt.Sprintf("action %q does not declare input %q", action.Command, name))
		}
	}
	targetName := stringConfig(input, "target_doctype", "doctype")
	var target *DocType
	if targetName != "" {
		target = checkDoc(path+".target_doctype", targetName)
	}
	childName := stringConfig(input, "child_table")
	var child *DocType
	if childName != "" && target != nil {
		if resolved := getDoc(childName); resolved != nil && resolved.IsChildTable && doctypeHasChildTable(target, resolved.Name) {
			child = resolved
		} else if tableField := target.GetField(childName); tableField != nil && strings.EqualFold(tableField.Fieldtype, "Table") {
			child = checkDoc(path+".child_table", tableField.Options)
		} else {
			child = checkDoc(path+".child_table", childName)
			if child != nil && !child.IsChildTable {
				add("reference.child_doctype_incompatible", path+".child_table", "doctype", child.Name, "action child_table must reference a child DocType or parent Table field")
			}
		}
		if child != nil && !doctypeHasChildTable(target, child.Name) {
			add("reference.child_table_unattached", path+".child_table", "doctype", child.Name, fmt.Sprintf("DocType %q has no Table field referencing child DocType %q", target.Name, child.Name))
		}
	}
	if childName != "" && target == nil {
		child = checkDoc(path+".child_table", childName)
	}
	if child == nil && target != nil && action.Command == "create_transaction" && (input["line_fields"] != nil || input["line_defaults"] != nil || input["totals"] != nil || input["decrement_stock"] == true) {
		tableFields := target.TableFields()
		if len(tableFields) == 1 {
			child = checkDoc(path+".child_table", tableFields[0].Options)
		} else if len(tableFields) > 1 {
			add("reference.action_child_table_ambiguous", path+".child_table", "doctype", target.Name, "transaction with line configuration must identify child_table because the target has multiple Table fields")
		} else {
			add("reference.action_child_table_missing", path+".child_table", "doctype", target.Name, "transaction line configuration requires a Table field on the target DocType")
		}
	}
	for name, definition := range contract.Inputs {
		value, ok := input[name]
		if !ok {
			continue
		}
		text, isString := value.(string)
		if !isString || strings.TrimSpace(text) == "" {
			continue
		}
		switch definition.Reference {
		case "doctype":
			if name != "target_doctype" {
				checkDoc(path+"."+name, text)
			}
		case "target_field", "target_numeric_field":
			checkField(path+"."+name, target, text)
			if definition.Reference == "target_numeric_field" && target != nil {
				if field := target.GetField(text); field != nil && !isNumericFieldType(field.Fieldtype) {
					add("reference.action_quantity_field_type", path+"."+name, "field", target.Name+"."+text, "action input requires a numeric field")
				}
			}
		case "parent_field":
			checkField(path+"."+name, target, text)
			if target != nil {
				if field := target.GetField(text); field != nil && !strings.EqualFold(field.Fieldtype, "Table") {
					add("reference.action_parent_field_type", path+"."+name, "field", target.Name+"."+text, "parent_field must reference a Table field")
				}
			}
		case "route":
			if !strings.HasPrefix(text, "/") || strings.ContainsAny(text, "?#") {
				add("reference.action_route_invalid", path+"."+name, "route", text, "navigation route must be an absolute application path without query or fragment")
			}
		}
	}
	validateConfiguredFields(input["fields"], target, path+".fields", checkField, add)
	if input["decrement_stock"] == true && input["related_quantity_updates"] == nil {
		add("reference.action_quantity_updates_missing", path+".related_quantity_updates", "quantity_updates", "", "decrement_stock requires related_quantity_updates")
	}
	validateConfiguredFields(input["line_defaults"], child, path+".line_defaults", checkField, add)
	validateMappedFields(input["line_fields"], child, path+".line_fields", checkField, add)
	validateTransactionTotals(input["totals"], target, child, path+".totals", checkField, add)
	validateRelatedQuantityUpdates(input["related_quantity_updates"], child, path+".related_quantity_updates", checkDoc, checkField, add)
	validateTransactionLineRecords(input["line_records"], child, path+".line_records", getDoc, checkField, add)
	validateTransactionNumericUpdates(input["related_numeric_updates"], target, path+".related_numeric_updates", getDoc, checkField, add)
	validateTransactionRelatedStatus(input["required_related_status"], target, path+".required_related_status", getDoc, checkField, add)
	validateQuantityEffectInputs(action.Command, input, getDoc, add, path)
	return findings
}

func validateTransactionRelatedStatus(raw any, parent *DocType, path string, getDoc func(string) *DocType, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	items, ok := raw.([]any)
	if raw == nil {
		return
	}
	if !ok {
		add("reference.action_related_status_type", path, "related_status", "", "required_related_status must be an array")
		return
	}
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		if !ok {
			add("reference.action_related_status_item", itemPath, "related_status", "", "related status requirement must be an object")
			continue
		}
		dt := getDoc(stringConfig(item, "doctype"))
		if dt == nil {
			add("reference.action_related_status_doctype", itemPath+".doctype", "doctype", stringConfig(item, "doctype"), "related status doctype must exist")
		}
		checkField(itemPath+".record_field", parent, stringConfig(item, "record_field"))
		checkField(itemPath+".status_field", dt, stringConfig(item, "status_field"))
		for _, key := range []string{"doctype", "record_field", "status_field", "equals"} {
			if stringConfig(item, key) == "" {
				add("reference.action_input_missing", itemPath+"."+key, "action_input", key, "related status requirement is missing a required value")
			}
		}
	}
}

func validateTransactionLineRecords(raw any, line *DocType, path string, getDoc func(string) *DocType, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	items, ok := raw.([]any)
	if raw == nil {
		return
	}
	if !ok {
		add("reference.action_line_records_type", path, "line_records", "", "line_records must be an array")
		return
	}
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		if !ok {
			add("reference.action_line_record_type", itemPath, "line_record", "", "line record must be an object")
			continue
		}
		target := getDoc(stringConfig(item, "doctype"))
		if target == nil {
			add("reference.action_line_record_doctype", itemPath+".doctype", "doctype", stringConfig(item, "doctype"), "line record doctype is required and must exist")
		}
		fields, fieldsOK := item["fields"].(map[string]any)
		if !fieldsOK || len(fields) == 0 {
			add("reference.action_line_record_fields", itemPath+".fields", "field_map", "", "line record fields are required")
		} else {
			for name := range fields {
				checkField(itemPath+".fields."+name, target, name)
			}
		}
		quantity, exists := item["quantity"].(map[string]any)
		if exists {
			fieldName, sourceName := stringConfig(quantity, "field"), stringConfig(quantity, "source_field")
			checkField(itemPath+".quantity.field", target, fieldName)
			checkField(itemPath+".quantity.source_field", line, sourceName)
			if target != nil {
				if field := target.GetField(fieldName); field != nil && !isNumericFieldType(field.Fieldtype) {
					add("reference.action_line_record_quantity_type", itemPath+".quantity.field", "field", target.Name+"."+fieldName, "line record quantity target must be numeric")
				}
			}
			if line != nil {
				if field := line.GetField(sourceName); field != nil && !isNumericFieldType(field.Fieldtype) {
					add("reference.action_line_record_quantity_type", itemPath+".quantity.source_field", "field", line.Name+"."+sourceName, "line record quantity source must be numeric")
				}
			}
			if sign, err := strconv.ParseFloat(fmt.Sprint(quantity["sign"]), 64); err == nil && sign != 1 && sign != -1 {
				add("reference.action_line_record_sign", itemPath+".quantity.sign", "number", fmt.Sprint(quantity["sign"]), "line record quantity sign must be 1 or -1")
			}
			for _, key := range []string{"field", "source_field", "sign"} {
				if quantity[key] == nil || quantity[key] == "" {
					add("reference.action_input_missing", itemPath+".quantity."+key, "action_input", key, "line record quantity requires field, source_field, and sign")
				}
			}
		}
	}
}

func validateTransactionNumericUpdates(raw any, parent *DocType, path string, getDoc func(string) *DocType, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	items, ok := raw.([]any)
	if raw == nil {
		return
	}
	if !ok {
		add("reference.action_numeric_updates_type", path, "numeric_updates", "", "related_numeric_updates must be an array")
		return
	}
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		if !ok {
			add("reference.action_numeric_update_type", itemPath, "numeric_update", "", "numeric update must be an object")
			continue
		}
		target := getDoc(stringConfig(item, "doctype"))
		if target == nil {
			add("reference.action_numeric_update_doctype", itemPath+".doctype", "doctype", stringConfig(item, "doctype"), "numeric update doctype is required and must exist")
		}
		checkField(itemPath+".record_field", parent, stringConfig(item, "record_field"))
		fieldName := stringConfig(item, "field")
		checkField(itemPath+".field", target, fieldName)
		if target != nil {
			if field := target.GetField(fieldName); field != nil && !isNumericFieldType(field.Fieldtype) {
				add("reference.action_numeric_update_field_type", itemPath+".field", "field", target.Name+"."+fieldName, "numeric updates require a numeric target field")
			}
		}
		source := stringConfig(item, "source_field")
		if source != "" {
			checkField(itemPath+".source_field", parent, source)
			if parent != nil {
				if field := parent.GetField(source); field != nil && !isNumericFieldType(field.Fieldtype) {
					add("reference.action_numeric_update_source_type", itemPath+".source_field", "field", parent.Name+"."+source, "numeric update source must be numeric")
				}
			}
		} else if item["value"] == nil {
			add("reference.action_numeric_update_source", itemPath, "numeric_update", "", "numeric update requires source_field or value")
		}
	}
}

func validateConfiguredFields(raw any, target *DocType, path string, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	fields, ok := raw.(map[string]any)
	if raw == nil {
		return
	}
	if !ok {
		add("reference.action_field_map_type", path, "field_map", "", "configured fields must be an object mapping DocType field names to values")
		return
	}
	for name, value := range fields {
		checkField(path+"."+name, target, name)
		if target != nil {
			if field := target.GetField(name); field != nil && !valueMatchesFieldType(value, field.Fieldtype) {
				add("reference.action_field_value_type", path+"."+name, "field_value", target.Name+"."+name, fmt.Sprintf("value does not match field type %q", field.Fieldtype))
			}
		}
	}
}

func validateMappedFields(raw any, target *DocType, path string, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	fields, ok := raw.(map[string]any)
	if raw == nil {
		return
	}
	if !ok {
		add("reference.action_field_map_type", path, "field_map", "", "line_fields must be an object mapping child field names to source field names")
		return
	}
	for name, value := range fields {
		checkField(path+"."+name, target, name)
		if _, ok := value.(string); !ok || strings.TrimSpace(value.(string)) == "" {
			add("reference.action_field_mapping_type", path+"."+name, "field_mapping", name, "line field source must be a non-empty string")
		}
	}
}

func validateTransactionTotals(raw any, parent, child *DocType, path string, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	totals, ok := raw.(map[string]any)
	if raw == nil {
		return
	}
	if !ok {
		add("reference.action_totals_type", path, "totals", "", "totals must be an object")
		return
	}
	checkNumeric := func(doc *DocType, name, fieldPath string) {
		if name == "" {
			return
		}
		checkField(fieldPath, doc, name)
		if doc != nil {
			if field := doc.GetField(name); field != nil && !isNumericFieldType(field.Fieldtype) {
				add("reference.action_total_field_type", fieldPath, "field", doc.Name+"."+name, fmt.Sprintf("totals require a numeric field, got %q", field.Fieldtype))
			}
		}
	}
	for _, name := range []string{"subtotal_field", "discount_total_field", "tax_total_field", "total_field"} {
		checkNumeric(parent, stringConfig(totals, name), path+"."+name)
	}
	lines, ok := totals["lines"].(map[string]any)
	if rawLines, exists := totals["lines"]; exists && rawLines != nil && !ok {
		add("reference.action_totals_type", path+".lines", "totals", "", "totals.lines must be an object")
	}
	for _, name := range []string{"quantity_field", "unit_price_field", "discount_field", "tax_field", "line_total_field"} {
		checkNumeric(child, stringConfig(lines, name), path+".lines."+name)
	}
}

func validateRelatedQuantityUpdates(raw any, line *DocType, path string, checkDoc func(string, string) *DocType, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	config, ok := raw.(map[string]any)
	if raw == nil {
		return
	}
	if !ok {
		add("reference.action_quantity_updates_type", path, "quantity_updates", "", "related_quantity_updates must be an object")
		return
	}
	targetName := stringConfig(config, "target_doctype")
	target := checkDoc(path+".target_doctype", targetName)
	for _, key := range []string{"source_record_field", "source_quantity_field"} {
		checkField(path+"."+key, line, stringConfig(config, key))
	}
	quantityField := stringConfig(config, "target_quantity_field")
	checkField(path+".target_quantity_field", target, quantityField)
	if target != nil && quantityField != "" {
		if field := target.GetField(quantityField); field != nil && !isNumericFieldType(field.Fieldtype) {
			add("reference.action_quantity_field_type", path+".target_quantity_field", "field", target.Name+"."+quantityField, "target quantity field must be numeric")
		}
	}
	for _, key := range []string{"target_doctype", "source_record_field", "source_quantity_field", "target_quantity_field"} {
		if stringConfig(config, key) == "" {
			add("reference.action_input_missing", path+"."+key, "action_input", key, fmt.Sprintf("related quantity updates require %q", key))
		}
	}
}

func validateQuantityEffectInputs(command string, input map[string]any, getDoc func(string) *DocType, add func(string, string, string, string, string), path string) {
	if command != "create_quantity_effect" {
		return
	}
	root := getDoc(stringConfig(input, "target_doctype"))
	related := getDoc(stringConfig(input, "related_doctype"))
	quantityField := stringConfig(input, "related_quantity_field")
	for _, pair := range []struct {
		doc  *DocType
		name string
	}{
		{root, stringConfig(input, "delta_field")}, {related, quantityField},
	} {
		if pair.doc != nil && pair.name != "" {
			if field := pair.doc.GetField(pair.name); field != nil && !isNumericFieldType(field.Fieldtype) {
				add("reference.action_quantity_field_type", path, "field", pair.doc.Name+"."+pair.name, "quantity effect fields must be numeric")
			}
		}
	}
}

func pageValueMatches(typeName string, value any) bool {
	switch typeName {
	case "any":
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		switch value.(type) {
		case int, int32, int64, float32, float64, json.Number:
			return true
		default:
			return false
		}
	case "object":
		_, ok := value.(map[string]any)
		if !ok {
			_, ok = value.(map[string]string)
		}
		return ok
	case "array":
		switch value.(type) {
		case []any, []string:
			return true
		default:
			return false
		}
	case "string_array":
		switch values := value.(type) {
		case []string:
			return true
		case []any:
			for _, item := range values {
				if _, ok := item.(string); !ok {
					return false
				}
			}
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func valueMatchesFieldType(value any, fieldType string) bool {
	if text, ok := value.(string); ok {
		if strings.HasPrefix(text, "$today") || strings.HasPrefix(text, "$now") || strings.HasPrefix(text, "$") {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(fieldType)) {
	case "int", "integer", "float", "currency", "percent":
		switch value.(type) {
		case int, int32, int64, float32, float64, json.Number:
			return true
		case string:
			_, err := strconv.ParseFloat(strings.TrimSpace(value.(string)), 64)
			return err == nil
		default:
			return false
		}
	case "check", "checkbox", "boolean":
		_, ok := value.(bool)
		return ok
	case "data", "text", "small text", "long text", "code", "select", "link", "date", "datetime", "time", "attach", "attach image", "email", "phone", "url", "password":
		_, ok := value.(string)
		return ok
	case "table":
		switch value.(type) {
		case []any, []map[string]any:
			return true
		default:
			return false
		}
	default:
		return value != nil
	}
}

func isNumericFieldType(fieldType string) bool {
	switch strings.ToLower(strings.TrimSpace(fieldType)) {
	case "int", "integer", "float", "currency", "percent":
		return true
	default:
		return false
	}
}

func containsFold(values []string, candidate string) bool {
	for _, value := range values {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func stringMap(value any) (map[string]string, bool) {
	switch bindings := value.(type) {
	case map[string]string:
		return bindings, true
	case map[string]any:
		result := make(map[string]string, len(bindings))
		for key, raw := range bindings {
			text, ok := raw.(string)
			if !ok {
				return nil, false
			}
			result[key] = text
		}
		return result, true
	default:
		return nil, false
	}
}

func validateViewReferences(view *View, path string, viewDT *DocType, checkDoc func(string, string) *DocType, checkField func(string, *DocType, string), add func(string, string, string, string, string)) {
	resourceDocs := map[string]*DocType{}
	resourceByID := map[string]PageResource{}
	for i, resource := range view.Resources {
		resourcePath := fmt.Sprintf("%s.resources[%d]", path, i)
		if strings.TrimSpace(resource.ID) == "" {
			add("reference.resource_id_missing", resourcePath+".id", "resource", "", "resource ID is required")
		}
		if _, exists := resourceByID[resource.ID]; exists {
			add("reference.resource_id_duplicate", resourcePath+".id", "resource", resource.ID, "resource IDs must be unique")
		}
		resourceByID[resource.ID] = resource
		if resource.Query != "document.list" && resource.Query != "document.get" {
			add("reference.query_unsupported", resourcePath+".query", "query", resource.Query, fmt.Sprintf("query type %q is not supported by reference validation", resource.Query))
		}
		doctypeName, _ := resource.Params["doctype"].(string)
		dt := checkDoc(resourcePath+".params.doctype", doctypeName)
		resourceDocs[resource.ID] = dt
		if raw, ok := resource.Params["fields"].([]any); ok {
			for fieldIndex, value := range raw {
				if field, ok := value.(string); ok {
					checkField(fmt.Sprintf("%s.params.fields[%d]", resourcePath, fieldIndex), dt, field)
				}
			}
		}
		if filters, ok := resource.Params["filters"].([]any); ok {
			for filterIndex, rawFilter := range filters {
				if filter, ok := rawFilter.(map[string]any); ok {
					field, _ := filter["field"].(string)
					checkField(fmt.Sprintf("%s.params.filters[%d].field", resourcePath, filterIndex), dt, field)
				}
			}
		}
	}
	if _, exists := resourceByID["primary"]; !exists && strings.TrimSpace(view.SourceDocType) != "" {
		resourceByID["primary"] = pageResource("primary", view.SourceDocType)
		resourceDocs["primary"] = checkDoc(path+".source_doctype", view.SourceDocType)
	}
	var walk func([]ViewComponent, string)
	walk = func(components []ViewComponent, prefix string) {
		for i, component := range components {
			componentPath := fmt.Sprintf("%s.components[%s]", prefix, component.ID)
			dt := viewDT
			if component.SourceDocType != "" {
				dt = checkDoc(componentPath+".source_doctype", component.SourceDocType)
			}
			contract, known := PageComponentContractFor(component.Type)
			if !known {
				add("reference.component_missing", componentPath+".type", "component", component.Type, fmt.Sprintf("component %q is not registered by this Kora UI runtime", component.Type))
			} else {
				componentResources := map[string]*DocType{}
				for role, resourceID := range component.ResourceBindings {
					definition, declared := contract.Resources[role]
					if !declared {
						add("reference.component_resource_unsupported", componentPath+".resource_bindings."+role, "resource_role", role, fmt.Sprintf("component %q does not declare resource role %q", component.Type, role))
						continue
					}
					resource, exists := resourceByID[resourceID]
					if !exists {
						add("reference.resource_missing", componentPath+".resource_bindings."+role, "resource", resourceID, fmt.Sprintf("resource %q bound to role %q is not declared by this view", resourceID, role))
						continue
					}
					if definition.Query != "" && resource.Query != definition.Query {
						add("reference.component_resource_incompatible", componentPath+".resource_bindings."+role, "resource", resourceID, fmt.Sprintf("component resource role %q requires query %q", role, definition.Query))
					}
					componentResources[role] = resourceDocs[resourceID]
					if definition.MustMatchData && dt != nil && componentResources[role] != nil && !strings.EqualFold(dt.Name, componentResources[role].Name) {
						add("reference.component_resource_incompatible", componentPath+".resource_bindings."+role, "doctype", componentResources[role].Name, fmt.Sprintf("component resource role %q must use source DocType %q", role, dt.Name))
					}
				}
				for role, definition := range contract.Resources {
					if definition.Required && strings.TrimSpace(component.ResourceBindings[role]) == "" {
						add("reference.component_resource_required", componentPath+".resource_bindings."+role, "resource", role, fmt.Sprintf("component %q requires resource role %q", component.Type, role))
					}
				}
				if _, exists := componentResources["products"]; !exists && dt != nil {
					componentResources["products"] = dt
				}
				bindings := make(map[string]any, len(component.Bindings))
				for name, field := range component.Bindings {
					bindings[name] = field
				}
				pageComponent := PageComponent{Component: component.Type, Version: contract.Version, Props: map[string]any{"bindings": bindings}, ResourceBindings: component.ResourceBindings}
				validateComponentRelations(pageComponent, componentPath, contract, componentResources, add)
				validateComponentContract(pageComponent, componentPath, contract, true, dt, componentResources, checkDoc, checkField, add)
			}
			for fi, filter := range component.Filters {
				checkField(fmt.Sprintf("%s.filters[%d].field", componentPath, fi), dt, filter.Field)
			}
			for _, field := range append(append([]string{}, component.DesktopColumns...), component.MobileColumns...) {
				checkField(componentPath+".columns", dt, field)
			}
			for ai, action := range component.Actions {
				ap := fmt.Sprintf("%s.actions[%d]", componentPath, ai)
				config := action.Config
				if config == nil {
					continue
				}
				targetName := stringConfig(config, "target_doctype", "doctype")
				target := checkDoc(ap+".config.target_doctype", targetName)
				childName := stringConfig(config, "child_table")
				if childName != "" {
					child := checkDoc(ap+".config.child_table", childName)
					if child != nil && !child.IsChildTable {
						add("reference.child_doctype_incompatible", ap+".config.child_table", "doctype", child.Name, "action child_table must reference a child DocType")
					}
					if child != nil && target != nil && !doctypeHasChildTable(target, child.Name) {
						add("reference.child_table_unattached", ap+".config.child_table", "doctype", child.Name, fmt.Sprintf("DocType %q has no Table field referencing child DocType %q", target.Name, child.Name))
					}
				}
				if fields, ok := config["line_fields"].(map[string]any); ok && childName != "" {
					child := checkDoc(ap+".config.child_table", childName)
					for outputField := range fields {
						checkField(ap+".config.line_fields."+outputField, child, outputField)
					}
				}
				if defaults, ok := config["line_defaults"].(map[string]any); ok && childName != "" {
					child := checkDoc(ap+".config.child_table", childName)
					for field := range defaults {
						checkField(ap+".config.line_defaults."+field, child, field)
					}
				}
				if totals, ok := config["totals"].(map[string]any); ok {
					checkField(ap+".config.totals.total_field", target, stringConfig(totals, "total_field"))
					if lines, ok := totals["lines"].(map[string]any); ok && childName != "" {
						child := checkDoc(ap+".config.child_table", childName)
						checkField(ap+".config.totals.lines.quantity_field", child, stringConfig(lines, "quantity_field"))
						checkField(ap+".config.totals.lines.unit_price_field", child, stringConfig(lines, "unit_price_field"))
					}
				}
			}
			walk(component.Components, componentPath+fmt.Sprintf("[%d]", i))
		}
	}
	walk(view.Components, path)
}

func stringConfig(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func doctypeHasChildTable(parent *DocType, child string) bool {
	if parent == nil {
		return false
	}
	for _, field := range parent.Fields {
		if strings.EqualFold(field.Fieldtype, "Table") && referenceKey(field.Options) == referenceKey(child) {
			return true
		}
	}
	return false
}
func referenceKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}
func normalizeViewRoute(value string) string {
	route := "/" + strings.Trim(strings.TrimSpace(value), "/")
	if route == "//" {
		return "/"
	}
	return route
}
func normalizeRouteSlug(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), " ", "-")
}
func candidateDocTypes(snapshot *ConfigSnapshot) []*DocType {
	if snapshot == nil {
		return nil
	}
	return snapshot.DocTypes
}
func liveDocTypes(registry *Registry) []*DocType {
	if registry == nil {
		return nil
	}
	return registry.All()
}
func isReferenceSystemField(field string) bool {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "name", "owner", "creation", "modified", "modified_by", "doc_status", "idx", "parent", "parentfield", "parenttype":
		return true
	default:
		return false
	}
}

func normalizeReferenceFindings(findings []ReferenceFinding) []ReferenceFinding {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Path == findings[j].Path {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Path < findings[j].Path
	})
	return findings
}
