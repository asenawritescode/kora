package ai

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

// executeListViews returns all views for the current site.
func executeListViews(reg *doctype.Registry) string {
	views := reg.Views.All()
	if len(views) == 0 {
		return "No views configured."
	}
	var lines []string
	for _, v := range views {
		lines = append(lines, fmt.Sprintf("%s (route: %s, type: %s, layout: %s, %d components)",
			v.Name, v.Route, v.Type, v.Layout, len(v.Components)))
	}
	return fmt.Sprintf("Views:\n%s", joinLines(lines))
}

// executeGetView returns a single view's full configuration as JSON.
func executeGetView(reg *doctype.Registry, name string) string {
	v := reg.Views.GetByName(name)
	if v == nil {
		return fmt.Sprintf("View %q not found.", name)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error serializing view: %v", err)
	}
	return string(data)
}

// executeCreateViewDraft creates a new view as a Draft config version.
// The AI provides the complete view JSON (same format as the REST API accepts).
func executeCreateViewDraft(tx *orm.TxManager, reg *doctype.Registry, viewJSON string, owner, siteName string) string {
	if viewJSON == "" {
		return "Error: no view configuration provided. Use the view JSON format: {\"name\":\"...\",\"route\":\"...\",\"type\":\"...\",\"components\":[...]}"
	}

	var view doctype.View
	if err := json.Unmarshal([]byte(viewJSON), &view); err != nil {
		return fmt.Sprintf("Error parsing view JSON: %v. Check your JSON syntax.", err)
	}

	// Validate structurally.
	if err := view.Validate(); err != nil {
		return fmt.Sprintf("View validation failed: %v", err)
	}

	// Validate against registry.
	if errs := validateViewAgainstReg(&view, reg); len(errs) > 0 {
		return fmt.Sprintf("View validation failed:\n%s", joinLines(errs))
	}

	store := configstore.NewStore(tx.DB, tx.Dialect)
	if err := store.SaveView(&view, siteName); err != nil {
		return fmt.Sprintf("Error saving view: %v", err)
	}

	// Create a Draft config version.
	snapshot, err := store.CollectSnapshot(reg, siteName)
	if err != nil {
		return fmt.Sprintf("Error collecting snapshot: %v", err)
	}
	if owner == "" || owner == "mcp-agent" {
		owner = "ai-assistant"
	}
	verID, verNum, err := store.CreateConfigVersion(
		siteName, owner, "Created view "+view.Name+" via AI (Draft)", "Draft", snapshot,
	)
	if err != nil {
		slog.Warn("config version creation failed", "error", err)
	}

	return fmt.Sprintf(
		"✓ Created view %q as DRAFT (%d components, route: %s). Version #%d (ID: %s). A human must review and activate it at /workspace/admin/versions before it takes effect. Preview at /workspace/pages/%s after activation.",
		view.Name, len(view.Components), view.Route, verNum, verID, view.Route,
	)
}

// viewUpdate is a new definition for an existing view.
type viewUpdate struct {
	Name     string
	ViewJSON string
	Owner    string
	Site     string
}

// executeUpdateViewDraft updates an existing view as a Draft config version.
func executeUpdateViewDraft(tx *orm.TxManager, reg *doctype.Registry, upd viewUpdate) string {
	name, viewJSON, owner, siteName := upd.Name, upd.ViewJSON, upd.Owner, upd.Site
	if name == "" || viewJSON == "" {
		return "Error: both name and view configuration are required."
	}

	var view doctype.View
	if err := json.Unmarshal([]byte(viewJSON), &view); err != nil {
		return fmt.Sprintf("Error parsing view JSON: %v", err)
	}
	view.Name = name // Enforce name consistency.

	if err := view.Validate(); err != nil {
		return fmt.Sprintf("View validation failed: %v", err)
	}

	if errs := validateViewAgainstReg(&view, reg); len(errs) > 0 {
		return fmt.Sprintf("View validation failed:\n%s", joinLines(errs))
	}

	store := configstore.NewStore(tx.DB, tx.Dialect)

	// Check the view exists.
	existing, err := store.LoadView(name, siteName)
	if err != nil || existing == nil {
		return fmt.Sprintf("Error: View %q does not exist. Use create_view to create new views.", name)
	}

	if err := store.SaveView(&view, siteName); err != nil {
		return fmt.Sprintf("Error saving view: %v", err)
	}

	snapshot, err := store.CollectSnapshot(reg, siteName)
	if err != nil {
		return fmt.Sprintf("Error collecting snapshot: %v", err)
	}
	if owner == "" || owner == "mcp-agent" {
		owner = "ai-assistant"
	}
	verID, verNum, err := store.CreateConfigVersion(
		siteName, owner, "Updated view "+view.Name+" via AI (Draft)", "Draft", snapshot,
	)
	if err != nil {
		slog.Warn("config version creation failed", "error", err)
	}

	return fmt.Sprintf(
		"✓ Updated view %q as DRAFT (%d components). Version #%d (ID: %s). A human must review and activate it before changes take effect.",
		view.Name, len(view.Components), verNum, verID,
	)
}

// executeValidateView validates a view JSON against the registry.
func executeValidateView(viewJSON string, reg *doctype.Registry) string {
	if viewJSON == "" {
		return "Error: no view configuration provided."
	}

	var view doctype.View
	if err := json.Unmarshal([]byte(viewJSON), &view); err != nil {
		return fmt.Sprintf("Error parsing view JSON: %v", err)
	}

	if err := view.Validate(); err != nil {
		return fmt.Sprintf("Invalid: %v", err)
	}

	if errs := validateViewAgainstReg(&view, reg); len(errs) > 0 {
		return fmt.Sprintf("Invalid:\n%s", joinLines(errs))
	}

	return "Valid."
}

// validateViewAgainstReg checks component bindings against the doctype registry.
func validateViewAgainstReg(view *doctype.View, reg *doctype.Registry) []string {
	var errors []string
	for i := range view.Components {
		comp := &view.Components[i]
		errors = append(errors, validateComp(comp, reg)...)
	}
	return errors
}

func validateComp(comp *doctype.ViewComponent, reg *doctype.Registry) []string {
	var errors []string
	prefix := fmt.Sprintf("component %q", comp.ID)

	if comp.SourceDocType != "" {
		dt := reg.Get(comp.SourceDocType)
		if dt == nil {
			errors = append(errors, fmt.Sprintf("%s: source doctype %q not found", prefix, comp.SourceDocType))
		} else {
			for prop, fieldName := range comp.Bindings {
				if f := dt.GetField(fieldName); f == nil {
					if !isSystemField(fieldName) {
						errors = append(errors, fmt.Sprintf("%s: binding %q references unknown field %q on %s",
							prefix, prop, fieldName, comp.SourceDocType))
					}
				}
			}
		}
	}

	for i := range comp.Components {
		errors = append(errors, validateComp(&comp.Components[i], reg)...)
	}
	return errors
}

func isSystemField(name string) bool {
	switch name {
	case "name", "owner", "creation", "modified", "modified_by", "doc_status", "idx":
		return true
	default:
		return false
	}
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	result := ""
	for i, l := range lines {
		if i > 0 {
			result += "\n"
		}
		result += l
	}
	return result
}
