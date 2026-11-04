// Package doctype provides the core types for Kigae's config-driven view system.
//
// Views are first-class config objects that define how DocTypes are presented
// to users. A View specifies a route, layout, and a tree of components — each
// with field bindings, filters, actions, and visibility rules.
//
// Views are versioned alongside doctypes in _kora_config_version snapshots.
// They are the current application-definition primitive in this repo. Page
// manifests are a projection of these views into the newer manifest contract,
// but the engine still executes through DocType/View/config-store plumbing
// rather than a fully generic RFC resource runtime.
package doctype

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// View defines a named screen/route in the application.
// It maps a URL route to a layout populated by components that bind to DocTypes.
type View struct {
	Name          string            `json:"name"           yaml:"name"`
	Route         string            `json:"route"          yaml:"route"`
	Type          string            `json:"type"           yaml:"type"`
	Layout        string            `json:"layout"         yaml:"layout"`
	Label         string            `json:"label"          yaml:"label"`
	Module        string            `json:"module"         yaml:"module"`
	SourceDocType string            `json:"source_doctype" yaml:"source_doctype,omitempty"`
	Task          *ViewTask         `json:"task,omitempty" yaml:"task,omitempty"`
	Resources     []PageResource    `json:"resources,omitempty" yaml:"resources,omitempty"`
	Components    []ViewComponent   `json:"components"     yaml:"components"`
	PublicAccess  *ViewPublicAccess `json:"public_access"  yaml:"public_access,omitempty"`
	PageManifest  *PageManifest     `json:"page_manifest,omitempty" yaml:"page_manifest,omitempty"`
}

// Validate checks that the view definition is structurally valid.
// It does NOT validate bindings against actual DocTypes — that requires
// a Registry and is done by the API validate endpoint.
func (v *View) Validate() error {
	if v.Name == "" {
		return fmt.Errorf("view name is required")
	}
	if v.Route == "" {
		return fmt.Errorf("view route is required for %q", v.Name)
	}
	if v.Type == "" {
		return fmt.Errorf("view type is required for %q", v.Name)
	}

	// Validate public access configuration.
	if v.PublicAccess != nil {
		if err := v.PublicAccess.Validate(); err != nil {
			return fmt.Errorf("view %q: %w", v.Name, err)
		}
	}
	if v.Task != nil {
		if err := v.Task.Validate(); err != nil {
			return fmt.Errorf("view %q: %w", v.Name, err)
		}
	}

	// Validate components.
	seenIDs := make(map[string]bool)
	for i := range v.Components {
		comp := &v.Components[i]
		if err := comp.Validate(seenIDs); err != nil {
			return fmt.Errorf("view %q, component %q: %w", v.Name, comp.ID, err)
		}
	}

	// Normalize defaults after validation passes.
	v.Normalize()
	return nil
}

// ViewTask records why a custom view exists in the user's work. Ordinary
// table routes and saved filters do not need a task contract; distinct
// operational workflows should declare one so their purpose can be reviewed.
type ViewTask struct {
	User             string   `json:"user" yaml:"user"`
	Trigger          string   `json:"trigger" yaml:"trigger"`
	Outcome          string   `json:"outcome" yaml:"outcome"`
	Reads            []string `json:"reads" yaml:"reads"`
	Writes           []string `json:"writes,omitempty" yaml:"writes,omitempty"`
	CompletionSignal string   `json:"completion_signal" yaml:"completion_signal"`
}

func (t *ViewTask) Validate() error {
	if strings.TrimSpace(t.User) == "" {
		return fmt.Errorf("task.user is required")
	}
	if strings.TrimSpace(t.Trigger) == "" {
		return fmt.Errorf("task.trigger is required")
	}
	if strings.TrimSpace(t.Outcome) == "" {
		return fmt.Errorf("task.outcome is required")
	}
	if len(t.Reads) == 0 {
		return fmt.Errorf("task.reads must name at least one record type")
	}
	for _, name := range append(append([]string(nil), t.Reads...), t.Writes...) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("task record types must not be blank")
		}
	}
	if strings.TrimSpace(t.CompletionSignal) == "" {
		return fmt.Errorf("task.completion_signal is required")
	}
	return nil
}

// Normalize applies defaults and sorts fields for deterministic output.
func (v *View) Normalize() {
	if v.Layout == "" {
		v.Layout = "single"
	}
	if v.Module == "" {
		v.Module = "Workspace"
	}
	for i := range v.Components {
		v.Components[i].Normalize()
	}
}

// ---------------------------------------------------------------------------
// ViewComponent
// ---------------------------------------------------------------------------

// ViewComponent is an instance of a component type within a view.
// Components can nest — a dashboard_grid contains metric_cards, a split_view
// contains a queue and detail panel.
type ViewComponent struct {
	ID               string            `json:"id" yaml:"id"`
	Type             string            `json:"type" yaml:"type"`
	Region           string            `json:"region" yaml:"region"`
	Label            string            `json:"label,omitempty" yaml:"label,omitempty"`
	SourceDocType    string            `json:"source_doctype,omitempty" yaml:"source_doctype,omitempty"`
	Data             string            `json:"data,omitempty" yaml:"data,omitempty"`
	ResourceBindings map[string]string `json:"resource_bindings,omitempty" yaml:"resource_bindings,omitempty"`
	Bindings         map[string]string `json:"bindings,omitempty" yaml:"bindings,omitempty"`
	Filters          []ViewFilter      `json:"filters,omitempty" yaml:"filters,omitempty"`
	Actions          []ViewAction      `json:"actions,omitempty" yaml:"actions,omitempty"`
	Rules            []ViewRule        `json:"rules,omitempty" yaml:"rules,omitempty"`
	Components       []ViewComponent   `json:"components,omitempty" yaml:"components,omitempty"`
	DesktopColumns   []string          `json:"desktop_columns,omitempty" yaml:"desktop_columns,omitempty"`
	MobileColumns    []string          `json:"mobile_columns,omitempty" yaml:"mobile_columns,omitempty"`
	Position         int               `json:"position" yaml:"position"`
	Span             int               `json:"span,omitempty" yaml:"span,omitempty"`
}

// Validate checks structural validity of a single component.
func (c *ViewComponent) Validate(seenIDs map[string]bool) error {
	if c.ID == "" {
		return fmt.Errorf("component id is required")
	}
	if seenIDs[c.ID] {
		return fmt.Errorf("duplicate component id %q", c.ID)
	}
	seenIDs[c.ID] = true
	if c.Type == "" {
		return fmt.Errorf("component type is required")
	}
	if c.Region == "" {
		c.Region = "main"
	}

	if c.Span < 0 || c.Span > 12 {
		return fmt.Errorf("span must be 0-12, got %d", c.Span)
	}

	// Validate actions.
	for i := range c.Actions {
		if err := c.Actions[i].Validate(); err != nil {
			return fmt.Errorf("action %q: %w", c.Actions[i].ID, err)
		}
	}

	// Validate rules.
	for i := range c.Rules {
		if err := c.Rules[i].Validate(); err != nil {
			return fmt.Errorf("rule: %w", err)
		}
	}

	// Validate filters.
	for i := range c.Filters {
		if err := c.Filters[i].Validate(); err != nil {
			return fmt.Errorf("filter on %q: %w", c.Filters[i].Field, err)
		}
	}

	// Validate nested children recursively.
	childSeen := make(map[string]bool)
	for i := range c.Components {
		if err := c.Components[i].Validate(childSeen); err != nil {
			return fmt.Errorf("nested component: %w", err)
		}
	}

	return nil
}

// Normalize applies defaults to a component and its children.
func (c *ViewComponent) Normalize() {
	if c.Region == "" {
		c.Region = "main"
	}
	for i := range c.Components {
		c.Components[i].Normalize()
	}
}

// IsContainer returns true if this component type renders children.
func (c *ViewComponent) IsContainer() bool {
	switch c.Type {
	case "dashboard_grid", "split_view", "tabs", "form_section", "field_group":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// ViewAction
// ---------------------------------------------------------------------------

// ViewAction defines what happens on a user interaction within a view.
// The action Type is resolved SERVER-SIDE from stored config on execution —
// the client sends only the action ID and context data.
type ViewAction struct {
	ID      string         `json:"id" yaml:"id"`
	Trigger string         `json:"trigger" yaml:"trigger"`
	Type    string         `json:"type" yaml:"type"`
	Config  map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

var validActionTriggers = map[string]bool{
	"on_click":  true,
	"on_submit": true,
	"on_scan":   true,
	"on_drag":   true,
	"on_change": true,
}

var validActionTypes = map[string]bool{
	"create_record":               true,
	"update_record":               true,
	"delete_record":               true,
	"navigate":                    true,
	"local_cart_add":              true,
	"local_cart_remove":           true,
	"local_state_set":             true,
	"call_script":                 true,
	"call_webhook":                true,
	"create_transaction":          true,
	"create_quantity_effect":      true,
	"initiate_external_operation": true,
	"validate_external_operation": true,
}

// Validate checks structural validity of an action definition.
func (a *ViewAction) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("action id is required")
	}
	if a.Trigger == "" {
		return fmt.Errorf("action trigger is required")
	}
	if a.Type == "" {
		return fmt.Errorf("action type is required")
	}
	if !validActionTriggers[a.Trigger] {
		return fmt.Errorf("unknown trigger %q (valid: %s)", a.Trigger,
			strings.Join(mapKeys(validActionTriggers), ", "))
	}
	if !validActionTypes[a.Type] {
		return fmt.Errorf("unknown action type %q (valid: %s)", a.Type,
			strings.Join(mapKeys(validActionTypes), ", "))
	}
	return nil
}

// IsMutation returns true if this action type modifies server state.
func (a *ViewAction) IsMutation() bool {
	switch a.Type {
	case "create_record", "update_record", "delete_record",
		"create_transaction",
		"call_script", "call_webhook", "initiate_external_operation", "validate_external_operation":
		return true
	default:
		return false
	}
}

// IsLocal returns true if this action only modifies client-side state.
func (a *ViewAction) IsLocal() bool {
	switch a.Type {
	case "local_cart_add", "local_cart_remove", "local_state_set", "navigate":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// ViewRule
// ---------------------------------------------------------------------------

// ViewRule controls component behavior based on document or view state.
// Phase 1 uses structured conditions (field + op + value).
type ViewRule struct {
	Target    string        `json:"target" yaml:"target"`
	Condition ViewCondition `json:"condition" yaml:"condition"`
}

var validRuleTargets = map[string]bool{
	"visible":  true,
	"hidden":   true,
	"disabled": true,
	"readonly": true,
}

// Validate checks structural validity of a rule definition.
func (r *ViewRule) Validate() error {
	if r.Target == "" {
		return fmt.Errorf("rule target is required")
	}
	if !validRuleTargets[r.Target] {
		return fmt.Errorf("unknown rule target %q (valid: %s)", r.Target,
			strings.Join(mapKeys(validRuleTargets), ", "))
	}
	if r.Condition.Field == "" {
		return fmt.Errorf("condition field is required")
	}
	if r.Condition.Op == "" {
		return fmt.Errorf("condition op is required")
	}
	if !validConditionOps[r.Condition.Op] {
		return fmt.Errorf("unknown condition op %q (valid: %s)", r.Condition.Op,
			strings.Join(mapKeys(validConditionOps), ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// ViewCondition
// ---------------------------------------------------------------------------

// ViewCondition is a structured, safe condition expression for view rules.
type ViewCondition struct {
	Field string `json:"field" yaml:"field"`
	Op    string `json:"op" yaml:"op"`
	Value any    `json:"value" yaml:"value"`
}

var validConditionOps = map[string]bool{
	"equals":     true,
	"not_equals": true,
	"in":         true,
	"is_set":     true,
	"is_not_set": true,
	"gt":         true,
	"gte":        true,
	"lt":         true,
	"lte":        true,
}

// ---------------------------------------------------------------------------
// ViewFilter
// ---------------------------------------------------------------------------

// ViewFilter scopes data for a component.
type ViewFilter struct {
	Field string `json:"field" yaml:"field"`
	Op    string `json:"op" yaml:"op"`
	Value any    `json:"value" yaml:"value"`
}

var validFilterOps = map[string]bool{
	"equals":     true,
	"not_equals": true,
	"in":         true,
	"like":       true,
	"gt":         true,
	"gte":        true,
	"lt":         true,
	"lte":        true,
}

// Validate checks structural validity of a filter definition.
func (f *ViewFilter) Validate() error {
	if f.Field == "" {
		return fmt.Errorf("filter field is required")
	}
	if f.Op == "" {
		return fmt.Errorf("filter op is required")
	}
	if !validFilterOps[f.Op] {
		return fmt.Errorf("unknown filter op %q (valid: %s)", f.Op,
			strings.Join(mapKeys(validFilterOps), ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// ViewPublicAccess
// ---------------------------------------------------------------------------

// ViewPublicAccess allows unauthenticated access to specific view components.
// Public views bypass SiteGuard. Access is enforced server-side through a
// three-layer check:
//  1. View allows the component (components list or "*")
//  2. Component's source doctype has public access enabled
//  3. Component bindings only reference public fields
type ViewPublicAccess struct {
	Enabled        bool     `json:"enabled"          yaml:"enabled"`
	Components     []string `json:"components"       yaml:"components"`
	AllowMutations bool     `json:"allow_mutations"  yaml:"allow_mutations"`
}

// Validate checks structural validity of the public access config.
func (pa *ViewPublicAccess) Validate() error {
	if !pa.Enabled {
		return nil
	}
	if len(pa.Components) == 0 {
		return fmt.Errorf("public_access.components must list at least one component id or \"*\"")
	}
	hasStar := false
	for _, id := range pa.Components {
		if id == "*" {
			hasStar = true
		}
	}
	if hasStar && len(pa.Components) > 1 {
		return fmt.Errorf(`public_access.components: "*" must be the only entry when present`)
	}
	return nil
}

// AllowsComponent returns true if the given component ID is in the allowed set.
func (pa *ViewPublicAccess) AllowsComponent(id string) bool {
	if pa == nil || !pa.Enabled {
		return false
	}
	if len(pa.Components) == 1 && pa.Components[0] == "*" {
		return true
	}
	for _, cid := range pa.Components {
		if cid == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// YAML Parsing
// ---------------------------------------------------------------------------

// ParseViewFile parses a single view YAML file.
func ParseViewFile(path string) (*View, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading view file %s: %w", path, err)
	}
	return ParseViewYAML(data)
}

// ParseViewYAML parses a view from YAML data.
func ParseViewYAML(data []byte) (*View, error) {
	v := &View{}
	if err := yaml.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("parsing view: %w", err)
	}
	if v.Name == "" {
		return nil, fmt.Errorf("view has no name")
	}
	if v.Route == "" {
		return nil, fmt.Errorf("view %q has no route", v.Name)
	}
	return v, nil
}

// ParseViewsDirectory looks for view YAML files in a directory.
// Each file contains one view.
func ParseViewsDirectory(path string) ([]*View, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // No views directory is fine.
		}
		return nil, fmt.Errorf("read views directory %s: %w", path, err)
	}

	var views []*View
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}

		v, err := ParseViewFile(path + "/" + entry.Name())
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mapKeys returns sorted keys from a string→bool map for deterministic error messages.
func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// sortStrings sorts a small string slice in-place using insertion sort.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
