// Package doctype provides the current page-manifest projection over the
// DocType/view system.
//
// PageManifest is a real contract used by the admin UI and config store, but it
// is still a projection layered over the existing DocType-centric runtime. It
// is not yet the full generic application-definition runtime from the RFC.
package doctype

import (
	"fmt"
	"strings"
)

type PageManifest struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   PageManifestMetadata `json:"metadata"`
	Spec       PageManifestSpec     `json:"spec"`
}

type PageManifestMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Package string `json:"package"`
	Status  string `json:"status"`
	Hash    string `json:"hash,omitempty"`
}

type PageManifestSpec struct {
	Route        string             `json:"route"`
	Runtime      string             `json:"runtime"`
	Permissions  []string           `json:"permissions"`
	Capabilities []string           `json:"capabilities"`
	Offline      string             `json:"offline"`
	Resources    []PageResource     `json:"resources"`
	Actions      []PageAction       `json:"actions"`
	Layout       PageManifestLayout `json:"layout"`
}

type PageResource struct {
	ID        string         `json:"id"`
	Query     string         `json:"query"`
	Params    map[string]any `json:"params"`
	DependsOn []string       `json:"depends_on,omitempty"`
}

type PageAction struct {
	ID           string         `json:"id"`
	Command      string         `json:"command"`
	Input        map[string]any `json:"input"`
	Invalidate   []string       `json:"invalidate"`
	Offline      string         `json:"offline,omitempty"`
	Confirmation bool           `json:"confirmation,omitempty"`
}

type PageManifestLayout struct {
	Type     string          `json:"type"`
	Columns  int             `json:"columns,omitempty"`
	Children []PageComponent `json:"children"`
}

type PageComponent struct {
	ID                   string          `json:"id"`
	Component            string          `json:"component"`
	Version              int             `json:"version"`
	Region               string          `json:"region"`
	Position             int             `json:"position"`
	Span                 int             `json:"span,omitempty"`
	Props                map[string]any  `json:"props"`
	Data                 string          `json:"data,omitempty"`
	Actions              []string        `json:"actions,omitempty"`
	RequiredCapabilities []string        `json:"required_capabilities,omitempty"`
	Permissions          []string        `json:"permissions,omitempty"`
	Offline              string          `json:"offline,omitempty"`
	Children             []PageComponent `json:"children,omitempty"`
}

func (p *PageManifest) Validate() error {
	if p.APIVersion != "ui.kora.dev/v1" {
		return fmt.Errorf("apiVersion must be ui.kora.dev/v1")
	}
	if p.Kind != "Page" {
		return fmt.Errorf("kind must be Page")
	}
	if strings.TrimSpace(p.Metadata.Name) == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if strings.TrimSpace(p.Metadata.Version) == "" {
		return fmt.Errorf("metadata.version is required")
	}
	if strings.TrimSpace(p.Metadata.Package) == "" {
		return fmt.Errorf("metadata.package is required")
	}
	if !strings.HasPrefix(p.Spec.Route, "/") {
		return fmt.Errorf("spec.route must start with /")
	}
	if strings.TrimSpace(p.Spec.Runtime) == "" {
		return fmt.Errorf("spec.runtime is required")
	}
	if p.Spec.Layout.Type == "" {
		return fmt.Errorf("spec.layout.type is required")
	}
	return nil
}

func PageManifestFromView(v *View) *PageManifest {
	if v == nil {
		return nil
	}
	status := "draft"
	offline := "read_only"
	capabilities := []string{}
	if v.PublicAccess != nil {
		capabilities = append(capabilities, v.PublicAccess.Components...)
		if v.PublicAccess.Enabled {
			status = "active"
		}
		if v.PublicAccess.AllowMutations {
			offline = "queue_writes"
		}
	}
	resources := []PageResource{}
	resourceByDocType := make(map[string]string)
	if v.SourceDocType != "" {
		resources = append(resources, pageResource("primary", v.SourceDocType))
		resourceByDocType[v.SourceDocType] = "primary"
	}
	collectViewResources(v.Components, v.SourceDocType, &resources, resourceByDocType)
	children := pageComponentsFromView(v.Components)
	bindViewComponentResources(children, resourceByDocType)
	return &PageManifest{
		APIVersion: "ui.kora.dev/v1",
		Kind:       "Page",
		Metadata: PageManifestMetadata{
			Name:    v.Name,
			Version: "0.1.0",
			Package: packageFromModule(v.Module),
			Status:  status,
		},
		Spec: PageManifestSpec{
			Route:        v.Route,
			Runtime:      ">=2.0.0 <3.0.0",
			Permissions:  []string{},
			Capabilities: capabilities,
			Offline:      offline,
			Resources:    resources,
			Actions:      pageActionsFromView(v.Components, len(resources) > 0),
			Layout: PageManifestLayout{
				Type:     normalizePageLayout(v.Layout),
				Columns:  12,
				Children: children,
			},
		},
	}
}

func pageResource(id, doctypeName string) PageResource {
	return PageResource{
		ID:     id,
		Query:  "document.list",
		Params: map[string]any{"doctype": doctypeName, "limit": 50},
	}
}

func collectViewResources(components []ViewComponent, primaryDocType string, resources *[]PageResource, resourceByDocType map[string]string) {
	for _, component := range components {
		docTypeName := strings.TrimSpace(component.SourceDocType)
		if docTypeName != "" && docTypeName != primaryDocType {
			if _, exists := resourceByDocType[docTypeName]; !exists {
				resourceID := normalizeResourceName("", docTypeName)
				if resourceID == "primary" {
					resourceID = "doctype-primary"
				}
				resourceByDocType[docTypeName] = resourceID
				*resources = append(*resources, pageResource(resourceID, docTypeName))
			}
		}
		collectViewResources(component.Components, primaryDocType, resources, resourceByDocType)
	}
}

func bindViewComponentResources(components []PageComponent, resourceByDocType map[string]string) {
	for i := range components {
		component := &components[i]
		if component.Data == "" && componentNeedsDataBinding(component.Component) {
			if docTypeName, ok := component.Props["source_doctype"].(string); ok {
				if resourceID := resourceByDocType[strings.TrimSpace(docTypeName)]; resourceID != "" {
					component.Data = resourceID + ".data"
				}
			}
		}
		bindViewComponentResources(component.Children, resourceByDocType)
	}
}

func (p *PageManifest) EnsurePrimaryDataBindings() {
	if p == nil {
		return
	}
	primaryDoctype := primaryResourceDoctype(p.Spec.Resources)
	if primaryDoctype == "" {
		primaryDoctype = componentSourceDoctype(p.Spec.Layout.Children)
	}
	ensurePageComponentDataBindings(p.Spec.Layout.Children, primaryDoctype)
	if primaryDoctype == "" {
		return
	}
	hasPrimary := false
	for _, resource := range p.Spec.Resources {
		if resource.ID == "primary" {
			hasPrimary = true
			break
		}
	}
	if !hasPrimary {
		p.Spec.Resources = append([]PageResource{{
			ID:     "primary",
			Query:  "document.list",
			Params: map[string]any{"doctype": primaryDoctype, "limit": 50},
		}}, p.Spec.Resources...)
	}
}

func (p *PageManifest) ToView() *View {
	sourceDocType := ""
	if len(p.Spec.Resources) > 0 {
		if value, ok := p.Spec.Resources[0].Params["doctype"].(string); ok {
			sourceDocType = value
		}
	}
	return &View{
		Name:          p.Metadata.Name,
		Route:         p.Spec.Route,
		Type:          pageTypeForManifest(p),
		Layout:        p.Spec.Layout.Type,
		Label:         p.Metadata.Name,
		Module:        p.Metadata.Package,
		SourceDocType: sourceDocType,
		Components:    viewComponentsFromPage(p.Spec.Layout.Children),
		PublicAccess: &ViewPublicAccess{
			Enabled:        p.Metadata.Status == "active",
			Components:     p.Spec.Capabilities,
			AllowMutations: p.Spec.Offline == "queue_writes" || p.Spec.Offline == "full_slice",
		},
	}
}

func pageComponentsFromView(components []ViewComponent) []PageComponent {
	out := make([]PageComponent, 0, len(components))
	for _, c := range components {
		props := map[string]any{
			"title":           c.Label,
			"source_doctype":  c.SourceDocType,
			"bindings":        c.Bindings,
			"desktop_columns": c.DesktopColumns,
			"mobile_columns":  c.MobileColumns,
		}
		if len(c.Actions) > 0 {
			actions := make([]map[string]any, 0, len(c.Actions))
			for _, action := range c.Actions {
				actions = append(actions, map[string]any{
					"id":      action.ID,
					"trigger": action.Trigger,
					"type":    action.Type,
					"config":  action.Config,
				})
			}
			props["actions"] = actions
		}
		out = append(out, PageComponent{
			ID:        c.ID,
			Component: c.Type,
			Version:   1,
			Region:    c.Region,
			Position:  c.Position,
			Span:      c.Span,
			Props:     props,
			Data:      c.Data,
			Actions:   actionIDsFromView(c.Actions),
			Children:  pageComponentsFromView(c.Components),
		})
	}
	return out
}

func viewComponentsFromPage(components []PageComponent) []ViewComponent {
	out := make([]ViewComponent, 0, len(components))
	for _, c := range components {
		out = append(out, ViewComponent{
			ID:             c.ID,
			Type:           c.Component,
			Region:         c.Region,
			Label:          stringProp(c.Props, "title", c.Component),
			SourceDocType:  stringProp(c.Props, "source_doctype", ""),
			Data:           c.Data,
			Bindings:       stringMapProp(c.Props, "bindings"),
			Actions:        viewActionsFromIDs(c.Actions),
			DesktopColumns: stringSliceProp(c.Props, "desktop_columns"),
			MobileColumns:  stringSliceProp(c.Props, "mobile_columns"),
			Components:     viewComponentsFromPage(c.Children),
			Position:       c.Position,
			Span:           c.Span,
		})
	}
	return out
}

func primaryResourceDoctype(resources []PageResource) string {
	for _, resource := range resources {
		if resource.ID != "primary" {
			continue
		}
		if value, ok := resource.Params["doctype"].(string); ok {
			return value
		}
	}
	return ""
}

func componentSourceDoctype(components []PageComponent) string {
	for _, component := range components {
		if value, ok := component.Props["source_doctype"].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
		if value := componentSourceDoctype(component.Children); value != "" {
			return value
		}
	}
	return ""
}

func ensurePageComponentDataBindings(components []PageComponent, doctype string) {
	for i := range components {
		component := &components[i]
		if componentNeedsDataBinding(component.Component) && component.Data == "" && doctype != "" {
			component.Data = "primary.data"
			if component.Props == nil {
				component.Props = map[string]any{}
			}
			if value, ok := component.Props["source_doctype"].(string); !ok || strings.TrimSpace(value) == "" {
				component.Props["source_doctype"] = doctype
			}
		}
		if len(component.Children) > 0 {
			ensurePageComponentDataBindings(component.Children, doctype)
		}
	}
}

func componentNeedsDataBinding(component string) bool {
	switch component {
	case "record_table", "record_list", "record_cards", "record_form", "record_detail", "metric_card", "chart", "kanban_board", "calendar_view", "approval_queue", "product_grid", "category_tabs":
		return true
	default:
		return false
	}
}

func pageActionsFromView(components []ViewComponent, invalidatePrimary bool) []PageAction {
	out := []PageAction{}
	var walk func([]ViewComponent)
	walk = func(items []ViewComponent) {
		for _, component := range items {
			for _, action := range component.Actions {
				invalidate := []string{}
				if invalidatePrimary && action.IsMutation() {
					invalidate = []string{"primary"}
				}
				out = append(out, PageAction{
					ID:         action.ID,
					Command:    action.Type,
					Input:      action.Config,
					Invalidate: invalidate,
				})
			}
			walk(component.Components)
		}
	}
	walk(components)
	return out
}

func actionIDsFromView(actions []ViewAction) []string {
	out := make([]string, 0, len(actions))
	for _, action := range actions {
		out = append(out, action.ID)
	}
	return out
}

func viewActionsFromIDs(ids []string) []ViewAction {
	out := make([]ViewAction, 0, len(ids))
	for _, id := range ids {
		out = append(out, ViewAction{ID: id, Trigger: "on_click", Type: "command"})
	}
	return out
}

func normalizePageLayout(layout string) string {
	switch layout {
	case "two_panel", "three_panel", "grid":
		return layout
	default:
		return "single"
	}
}

func packageFromModule(module string) string {
	if strings.TrimSpace(module) == "" {
		return "tenant.workspace"
	}
	return "tenant." + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(module), " ", "-"))
}

func pageTypeForManifest(p *PageManifest) string {
	for _, resource := range p.Spec.Resources {
		if resource.Query == "document.list" {
			return "collection"
		}
	}
	if p.Spec.Layout.Type == "grid" {
		return "dashboard"
	}
	return "custom"
}

func stringProp(props map[string]any, key, fallback string) string {
	if value, ok := props[key].(string); ok {
		return value
	}
	return fallback
}

func stringMapProp(props map[string]any, key string) map[string]string {
	value, ok := props[key]
	if !ok || value == nil {
		return nil
	}
	if typed, ok := value.(map[string]string); ok {
		return typed
	}
	out := map[string]string{}
	if generic, ok := value.(map[string]any); ok {
		for k, v := range generic {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stringSliceProp(props map[string]any, key string) []string {
	value, ok := props[key]
	if !ok || value == nil {
		return nil
	}
	if typed, ok := value.([]string); ok {
		return typed
	}
	generic, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(generic))
	for _, entry := range generic {
		if s, ok := entry.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
