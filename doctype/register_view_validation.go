package doctype

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
)

// publishedOperationSurface is what a version 2 view may rely on from an
// owner-published operation.
type publishedOperationSurface struct {
	lineEntry, payment bool
	inputs             map[string]bool
}

// publishedSurfaceCatalog indexes the snapshot modules' published queries
// (by owner/name, to their allowed fields) and operations (by operation ref).
type publishedSurfaceCatalog struct {
	queries    map[string][]string
	operations map[string]publishedOperationSurface
}

func newPublishedSurfaceCatalog(modules []contract.ModuleDescriptor) publishedSurfaceCatalog {
	catalog := publishedSurfaceCatalog{queries: map[string][]string{}, operations: map[string]publishedOperationSurface{}}
	for _, module := range modules {
		for _, query := range module.Queries {
			catalog.queries[string(query.OwnerModuleID)+"/"+query.Name] = query.AllowedFields
		}
		for _, published := range module.PublishedOperations {
			surface := publishedOperationSurface{lineEntry: published.Presentation.LineEntry != nil, payment: published.Payment != nil, inputs: map[string]bool{}}
			addSchemaProperties(surface.inputs, module.Schemas, operationInput(module, published))
			catalog.operations[contract.PublishedOperationRef(module.ID, published.Capability, published.Operation)] = surface
		}
	}
	return catalog
}

func operationInput(module contract.ModuleDescriptor, published contract.PublishedOperation) contract.SchemaRef {
	for _, offer := range module.Provides {
		for _, operation := range offer.Operations {
			if offer.Name == published.Capability && operation.Name == published.Operation {
				return operation.Input
			}
		}
	}
	return contract.SchemaRef{}
}

func addSchemaProperties(into map[string]bool, schemas []contract.ModuleSchemaDocument, ref contract.SchemaRef) {
	for _, document := range schemas {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if document.Ref == ref && json.Unmarshal(document.Schema, &schema) == nil {
			for name := range schema.Properties {
				into[name] = true
			}
		}
	}
}

// publishedQueryResource is the view resource query kind for owner-published queries.
const publishedQueryResource = "published.query"

func publishedQueryKey(resource PageResource) string {
	owner, _ := resource.Params["owner_module_id"].(string)
	name, _ := resource.Params["query_name"].(string)
	return strings.TrimSpace(owner) + "/" + strings.TrimSpace(name)
}

// publishedComponentCheck validates one version 2 component against the
// published surfaces it binds. It never consults DocType definitions.
type publishedComponentCheck struct {
	component  ViewComponent
	path       string
	definition PageComponentContract
	resources  map[string]PageResource
	catalog    publishedSurfaceCatalog
	add        func(string, string, string, string, string)
}

func validatePublishedViewComponent(component ViewComponent, path string, definition PageComponentContract, resources map[string]PageResource, catalog publishedSurfaceCatalog, add func(string, string, string, string, string)) {
	check := publishedComponentCheck{component: component, path: path, definition: definition, resources: resources, catalog: catalog, add: add}
	check.resourceRoles()
	check.bindings()
	check.operation()
}

func (c publishedComponentCheck) field(fieldPath, resourceID, field string) {
	resource, exists := c.resources[resourceID]
	if !exists || resource.Query != publishedQueryResource {
		return
	}
	if fields, known := c.catalog.queries[publishedQueryKey(resource)]; known && !containsFold(fields, field) {
		c.add("reference.published_field_missing", fieldPath, "field", field, fmt.Sprintf("field %q is not an allowed field of the published query bound as %q", field, resourceID))
	}
}

func (c publishedComponentCheck) resourceRoles() {
	for role, resourceID := range c.component.ResourceBindings {
		definition, declared := c.definition.Resources[role]
		resource, exists := c.resources[resourceID]
		rolePath := c.path + ".resource_bindings." + role
		switch {
		case !declared:
			c.add("reference.component_resource_unsupported", rolePath, "resource_role", role, fmt.Sprintf("component %q does not declare resource role %q", c.component.Type, role))
		case !exists:
			c.add("reference.resource_missing", rolePath, "resource", resourceID, fmt.Sprintf("resource %q bound to role %q is not declared by this view", resourceID, role))
		case resource.Query != definition.Query:
			c.add("reference.component_resource_incompatible", rolePath, "resource", resourceID, fmt.Sprintf("component resource role %q requires query %q", role, definition.Query))
		}
	}
	for role, definition := range c.definition.Resources {
		if definition.Required && strings.TrimSpace(c.component.ResourceBindings[role]) == "" {
			c.add("reference.component_resource_required", c.path+".resource_bindings."+role, "resource", role, fmt.Sprintf("component %q requires resource role %q", c.component.Type, role))
		}
	}
}

func (c publishedComponentCheck) bindings() {
	for key, value := range c.component.Bindings {
		binding, declared := c.definition.Bindings[key]
		bindingPath := c.path + ".bindings." + key
		switch {
		case !declared:
			c.add("reference.component_binding_unsupported", bindingPath, "component_binding", key, fmt.Sprintf("component %q does not declare binding %q", c.component.Type, key))
		case binding.Kind == "field" && strings.TrimSpace(value) != "":
			c.field(bindingPath, c.component.ResourceBindings[binding.ResourceRole], strings.TrimSpace(value))
		case binding.Kind == "literal":
			if err := validateBindingValue(binding, value, nil, nil, bindingPath); err != "" {
				c.add("reference.component_binding_invalid", bindingPath, "component_binding", key, err)
			}
		}
	}
	for key, binding := range c.definition.Bindings {
		required := binding.Required || binding.RequiredWhenResourceRole != "" && c.component.ResourceBindings[binding.RequiredWhenResourceRole] != ""
		if required && strings.TrimSpace(c.component.Bindings[key]) == "" {
			c.add("reference.component_binding_required", c.path+".bindings."+key, "component_binding", key, fmt.Sprintf("component %q requires binding %q", c.component.Type, key))
		}
	}
}

// operation checks submit_operation_ref and the context inputs it receives.
func (c publishedComponentCheck) operation() {
	if _, declared := c.definition.Bindings["submit_operation_ref"]; !declared {
		if len(c.component.Context) > 0 {
			c.add("reference.component_binding_unsupported", c.path+".context", "component_binding", "context", fmt.Sprintf("component %q has no operation to receive context", c.component.Type))
		}
		return
	}
	ref := strings.TrimSpace(c.component.Bindings["submit_operation_ref"])
	operation, published := c.catalog.operations[ref]
	refPath := c.path + ".bindings.submit_operation_ref"
	if ref != "" && !published {
		c.add("reference.operation_missing", refPath, "operation", ref, fmt.Sprintf("operation %q is not published by an installed module", ref))
	}
	if published && (!operation.lineEntry || !operation.payment) {
		c.add("reference.operation_incompatible", refPath, "operation", ref, "submit operation must publish both presentation.line_entry and payment")
	}
	for input, source := range c.component.Context {
		contextPath := c.path + ".context." + input
		if published && !operation.inputs[input] {
			c.add("reference.operation_input_missing", contextPath, "operation_input", input, fmt.Sprintf("%q is not an input of operation %q", input, ref))
		}
		resourceID, field, _ := strings.Cut(strings.TrimSpace(source), ".")
		resource, exists := c.resources[resourceID]
		switch {
		case !exists || field == "":
			c.add("reference.resource_missing", contextPath, "resource", source, "context source must be <resource_id>.<field> of a declared published.query resource")
		case resource.Query != publishedQueryResource:
			c.add("reference.component_resource_incompatible", contextPath, "resource", resourceID, "context source must be a published.query resource")
		default:
			c.field(contextPath, resourceID, field)
		}
	}
}
