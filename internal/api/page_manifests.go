package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
)

type pageManifestListEntry struct {
	Name          string `json:"name"`
	Route         string `json:"route"`
	Layout        string `json:"layout"`
	Label         string `json:"label"`
	Module        string `json:"module"`
	Status        string `json:"status"`
	Purpose       string `json:"purpose"`
	SourceDocType string `json:"source_doctype,omitempty"`
	TaskOutcome   string `json:"task_outcome,omitempty"`
}

func savePageManifestDraft(store *configstore.Store, manifest *doctype.PageManifest, site, actor, label string, snapshot *doctype.ConfigSnapshot) (string, int, error) {
	if store == nil || snapshot == nil {
		return "", 0, fmt.Errorf("page manifest store and snapshot are required")
	}
	tx, err := store.DB.Begin()
	if err != nil {
		return "", 0, fmt.Errorf("beginning page manifest draft transaction: %w", err)
	}
	defer tx.Rollback()
	if err := store.SavePageManifestTx(tx, manifest, site, 0); err != nil {
		return "", 0, err
	}
	versionID, versionNum, err := store.CreateConfigVersionTx(tx, site, actor, label, "Draft", snapshot, "")
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("committing page manifest draft: %w", err)
	}
	return versionID, versionNum, nil
}

// HandleSystemPageManifests lists RFC-native page manifests.
// GET /api/v1/system/page-manifests
func (h *Handler) HandleSystemPageManifests(c *gin.Context) {
	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "page manifest store not available", nil)
		return
	}

	manifests, err := store.LoadPageManifests(site)
	if err != nil {
		internalError(c, "loading page manifests", err)
		return
	}

	items := make([]pageManifestListEntry, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest == nil {
			continue
		}
		items = append(items, pageManifestListEntry{
			Name:          manifest.Metadata.Name,
			Route:         manifest.Spec.Route,
			Layout:        manifest.Spec.Layout.Type,
			Label:         manifest.Metadata.Name,
			Module:        manifest.Metadata.Package,
			Status:        manifest.Metadata.Status,
			Purpose:       pageManifestPurpose(manifest),
			SourceDocType: pageManifestSourceDocType(manifest),
			TaskOutcome:   pageManifestTaskOutcome(manifest),
		})
	}

	c.Header("ETag", pageManifestETag(items))
	c.JSON(http.StatusOK, Response{Data: items})
}

func pageManifestTaskOutcome(manifest *doctype.PageManifest) string {
	if manifest == nil || manifest.Spec.Task == nil {
		return ""
	}
	return strings.TrimSpace(manifest.Spec.Task.Outcome)
}

// pageManifestPurpose keeps a plain table listing from being presented as a
// second destination when the doctype already has its spreadsheet page. A
// screen earns a separate navigation entry when it adds a distinct workflow
// or combines more than one view/component.
func pageManifestPurpose(manifest *doctype.PageManifest) string {
	if manifest == nil || len(manifest.Spec.Actions) > 0 || len(manifest.Spec.Layout.Children) != 1 {
		return "workflow"
	}
	component := manifest.Spec.Layout.Children[0]
	if component.Component != "record_table" || len(component.Children) > 0 || len(component.Actions) > 0 {
		return "workflow"
	}
	if actions, ok := component.Props["actions"].([]any); ok && len(actions) > 0 {
		return "workflow"
	}
	if len(manifest.Spec.Resources) != 1 || manifest.Spec.Resources[0].Query != "document.list" {
		return "workflow"
	}
	resource := manifest.Spec.Resources[0]
	if component.Data != "" && component.Data != resource.ID+".data" {
		return "workflow"
	}
	doctypeName, ok := resource.Params["doctype"].(string)
	if source, sourceOK := component.Props["source_doctype"].(string); sourceOK && source != "" {
		if !ok || doctypeName != source {
			return "workflow"
		}
	}
	if !ok || strings.TrimSpace(doctypeName) == "" {
		return "workflow"
	}
	if hasPageManifestFilters(resource.Params["filters"]) {
		return "table_view"
	}
	return "record_list"
}

func pageManifestSourceDocType(manifest *doctype.PageManifest) string {
	if manifest == nil {
		return ""
	}
	for _, resource := range manifest.Spec.Resources {
		if resource.Query == "document.list" {
			if name, ok := resource.Params["doctype"].(string); ok {
				return strings.TrimSpace(name)
			}
		}
	}
	return ""
}

func hasPageManifestFilters(value any) bool {
	switch filters := value.(type) {
	case []any:
		return len(filters) > 0
	case []map[string]any:
		return len(filters) > 0
	case string:
		return strings.TrimSpace(filters) != "" && strings.TrimSpace(filters) != "[]" && strings.TrimSpace(filters) != "null"
	default:
		return false
	}
}

// HandleSystemPageManifest returns a single RFC page manifest by name.
// GET /api/v1/system/page-manifests/:name
func (h *Handler) HandleSystemPageManifest(c *gin.Context) {
	name := c.Param("name")
	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "page manifest store not available", nil)
		return
	}

	manifest, err := store.LoadPageManifest(name, site)
	if err != nil {
		writeError(c, http.StatusNotFound, "page_manifest.not_found", "Page manifest not found", map[string]any{"name": name})
		return
	}
	normalizeManifestBindings(manifest, h.siteRegistry(c))

	c.Header("ETag", pageManifestETag(manifest))
	c.JSON(http.StatusOK, Response{Data: manifest})
}

// HandleSystemPageManifestCreate creates a new RFC page manifest.
// POST /api/v1/system/page-manifests
func (h *Handler) HandleSystemPageManifestCreate(c *gin.Context) {
	var manifest doctype.PageManifest
	if err := c.ShouldBindJSON(&manifest); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid page manifest JSON: "+err.Error(), nil)
		return
	}

	if err := manifest.Validate(); err != nil {
		badRequestError(c, "validation.failed", err.Error(), nil)
		return
	}
	manifest.EnsurePrimaryDataBindings()

	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "page manifest store not available", nil)
		return
	}
	if rejectInvalidPageManifestReferences(c, &manifest, h.siteRegistry(c)) {
		return
	}
	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}
	upsertView(snapshot, manifest.ToView())
	versionID, versionNum, err := savePageManifestDraft(store, &manifest, site, currentUser(c), "Created page manifest "+manifest.Metadata.Name, snapshot)
	if err != nil {
		internalError(c, "saving page manifest draft", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"manifest":    manifest,
		"version_id":  versionID,
		"version_num": versionNum,
		"status":      "Draft",
	}})
}

// HandleSystemPageManifestUpdate updates an existing RFC page manifest.
// PUT /api/v1/system/page-manifests/:name
func (h *Handler) HandleSystemPageManifestUpdate(c *gin.Context) {
	name := c.Param("name")
	var manifest doctype.PageManifest
	if err := c.ShouldBindJSON(&manifest); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid page manifest JSON: "+err.Error(), nil)
		return
	}
	manifest.Metadata.Name = name

	if err := manifest.Validate(); err != nil {
		badRequestError(c, "validation.failed", err.Error(), nil)
		return
	}

	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "page manifest store not available", nil)
		return
	}
	existing, err := store.LoadPageManifest(name, site)
	if err != nil {
		writeError(c, http.StatusNotFound, "page_manifest.not_found", "Page manifest not found", map[string]any{"name": name})
		return
	}

	manifest.EnsurePrimaryDataBindings()
	existing.EnsurePrimaryDataBindings()
	if pageManifestEquivalent(existing, &manifest) {
		c.JSON(http.StatusOK, Response{Data: map[string]any{
			"manifest":    existing,
			"version_id":  "",
			"version_num": 0,
			"status":      existing.Metadata.Status,
		}})
		return
	}
	if rejectInvalidPageManifestReferences(c, &manifest, h.siteRegistry(c)) {
		return
	}

	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}
	upsertView(snapshot, manifest.ToView())
	versionID, versionNum, err := savePageManifestDraft(store, &manifest, site, currentUser(c), "Updated page manifest "+manifest.Metadata.Name, snapshot)
	if err != nil {
		internalError(c, "saving page manifest draft", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"manifest":    manifest,
		"version_id":  versionID,
		"version_num": versionNum,
		"status":      "Draft",
	}})
}

// HandleSystemPageManifestDelete removes a page manifest by name.
// DELETE /api/v1/system/page-manifests/:name
func (h *Handler) HandleSystemPageManifestDelete(c *gin.Context) {
	name := c.Param("name")
	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "page manifest store not available", nil)
		return
	}

	if err := store.DeleteView(name, site); err != nil {
		internalError(c, "deleting page manifest", err)
		return
	}

	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}
	versionID, versionNum, err := store.CreateConfigVersion(site, currentUser(c), "Deleted page manifest "+name, "Draft", snapshot)
	if err != nil {
		internalError(c, "creating config version", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"version_id":  versionID,
		"version_num": versionNum,
		"status":      "Draft",
	}})
}

// HandlePageManifestByRoute resolves a manifest by runtime route.
// GET /api/v1/page-manifests?route=/orders
func (h *Handler) HandlePageManifestByRoute(c *gin.Context) {
	route := c.Query("route")
	if route == "" {
		badRequestError(c, "validation.required_field", "route query parameter is required", map[string]any{"field": "route"})
		return
	}
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}

	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "page manifest store not available", nil)
		return
	}
	manifests, err := store.LoadPageManifests(siteName(c))
	if err != nil {
		internalError(c, "loading page manifests", err)
		return
	}
	for _, manifest := range manifests {
		if manifest != nil && manifest.Spec.Route == route {
			normalizeManifestBindings(manifest, h.siteRegistry(c))
			manifest.EnsurePrimaryDataBindings()
			c.JSON(http.StatusOK, Response{Data: manifest})
			return
		}
	}
	notFoundError(c, "page_manifest.not_found", "Page manifest not found for route: "+route, map[string]any{"route": route})
}

// normalizeManifestBindings preserves the existing manifest format while
// resolving its meaning at the Engine boundary. A binding that names a field
// on the component's source DocType remains a field binding; an unknown value
// is marked as a literal for the generic Studio runtime. This keeps legacy
// manifests readable without component-specific frontend exceptions or new
// YAML syntax.
func normalizeManifestBindings(manifest *doctype.PageManifest, registry *doctype.Registry) {
	if manifest == nil || registry == nil {
		return
	}
	var visit func([]doctype.PageComponent)
	visit = func(components []doctype.PageComponent) {
		for i := range components {
			component := &components[i]
			source, _ := component.Props["source_doctype"].(string)
			var fields map[string]struct{}
			if dt := registry.Get(source); dt != nil {
				fields = map[string]struct{}{"name": {}}
				for _, field := range dt.Fields {
					fields[field.Fieldname] = struct{}{}
				}
			}
			bindings, ok := component.Props["bindings"].(map[string]any)
			if ok {
				for key, raw := range bindings {
					value, ok := raw.(string)
					if !ok || strings.HasPrefix(value, "literal:") || manifestLiteralBinding(key) {
						continue
					}
					if fields != nil && bindingMatchesFields(value, fields) {
						continue
					}
					if fields == nil || !bindingMatchesFields(value, fields) {
						bindings[key] = "literal:" + value
					}
				}
			}
			visit(component.Children)
		}
	}
	visit(manifest.Spec.Layout.Children)
}

func bindingMatchesFields(value string, fields map[string]struct{}) bool {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := fields[part]; !ok {
			return false
		}
	}
	return true
}

func manifestLiteralBinding(key string) bool {
	switch key {
	case "metric", "filter_value", "trend", "density", "variant", "format", "summary", "updated_at", "resource":
		return true
	default:
		return false
	}
}

func pageManifestETag(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func pageManifestEquivalent(a, b *doctype.PageManifest) bool {
	return bytes.Equal(canonicalPageManifestJSON(a), canonicalPageManifestJSON(b))
}

func canonicalPageManifestJSON(manifest *doctype.PageManifest) []byte {
	if manifest == nil {
		return []byte("null")
	}
	canonical := canonicalizePageManifest(*manifest)
	b, err := json.Marshal(canonical)
	if err != nil {
		return []byte("null")
	}
	return b
}

func canonicalizePageManifest(manifest doctype.PageManifest) doctype.PageManifest {
	manifest.Spec = canonicalizePageManifestSpec(manifest.Spec)
	return manifest
}

func canonicalizePageManifestSpec(spec doctype.PageManifestSpec) doctype.PageManifestSpec {
	spec.Permissions = canonicalStrings(spec.Permissions)
	spec.Capabilities = canonicalStrings(spec.Capabilities)
	spec.Resources = canonicalPageResources(spec.Resources)
	spec.Actions = canonicalPageActions(spec.Actions)
	spec.Layout = canonicalizePageManifestLayout(spec.Layout)
	return spec
}

func canonicalizePageManifestLayout(layout doctype.PageManifestLayout) doctype.PageManifestLayout {
	layout.Children = canonicalPageComponents(layout.Children)
	return layout
}

func canonicalPageResources(resources []doctype.PageResource) []doctype.PageResource {
	out := make([]doctype.PageResource, 0, len(resources))
	for _, resource := range resources {
		resource.Params = canonicalAnyMap(resource.Params)
		resource.DependsOn = canonicalStrings(resource.DependsOn)
		out = append(out, resource)
	}
	return out
}

func canonicalPageActions(actions []doctype.PageAction) []doctype.PageAction {
	out := make([]doctype.PageAction, 0, len(actions))
	for _, action := range actions {
		action.Input = canonicalAnyMap(action.Input)
		action.Invalidate = canonicalStrings(action.Invalidate)
		out = append(out, action)
	}
	return out
}

func canonicalPageComponents(components []doctype.PageComponent) []doctype.PageComponent {
	out := make([]doctype.PageComponent, 0, len(components))
	for _, component := range components {
		component.Props = canonicalAnyMap(component.Props)
		component.Actions = canonicalStrings(component.Actions)
		component.RequiredCapabilities = canonicalStrings(component.RequiredCapabilities)
		component.Permissions = canonicalStrings(component.Permissions)
		component.Children = canonicalPageComponents(component.Children)
		out = append(out, component)
	}
	return out
}

func canonicalStrings(values []string) []string {
	out := make([]string, 0, len(values))
	out = append(out, values...)
	return out
}

func canonicalAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = canonicalJSONValue(value)
	}
	return out
}

func canonicalJSONValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case map[string]any:
		return canonicalAnyMap(typed)
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[key] = entry
		}
		return out
	case []string:
		return canonicalStrings(typed)
	case []any:
		out := make([]any, 0, len(typed))
		for _, entry := range typed {
			out = append(out, canonicalJSONValue(entry))
		}
		return out
	case []doctype.PageComponent:
		return canonicalPageComponents(typed)
	case []doctype.PageResource:
		return canonicalPageResources(typed)
	case []doctype.PageAction:
		return canonicalPageActions(typed)
	default:
		return value
	}
}
