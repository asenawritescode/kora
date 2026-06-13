package doctype

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Workflow defines a document lifecycle with states and transitions.
type Workflow struct {
	Name               string              `yaml:"name"                json:"name"`
	DocumentType       string              `yaml:"document_type"       json:"document_type"`
	IsActive           bool                `yaml:"is_active"           json:"is_active"`
	WorkflowStateField string              `yaml:"workflow_state_field" json:"workflow_state_field"`
	States             []WorkflowState     `yaml:"states"              json:"states"`
	Transitions        []WorkflowTransition `yaml:"transitions"        json:"transitions"`
	Notifications      []WorkflowNotification `yaml:"notifications"   json:"notifications,omitempty"`
}

// WorkflowState defines a single state in a workflow.
type WorkflowState struct {
	State     string `yaml:"state"      json:"state"`
	DocStatus int    `yaml:"doc_status" json:"doc_status"`
	AllowEdit string `yaml:"allow_edit" json:"allow_edit"` // Role allowed to edit in this state
	Style     string `yaml:"style"      json:"style"`      // default | warning | success | danger | info
}

// WorkflowTransition defines a possible transition between states.
type WorkflowTransition struct {
	Action        string   `yaml:"action"         json:"action"`
	From          string   `yaml:"from"           json:"from"`
	To            string   `yaml:"to"             json:"to"`
	Allowed       string   `yaml:"allowed"        json:"allowed"`        // Role(s) allowed to perform
	Condition     string   `yaml:"condition"      json:"condition,omitempty"`
	RequireFields []string `yaml:"require_fields" json:"require_fields,omitempty"`
}

// WorkflowNotification defines a notification triggered by a state change.
type WorkflowNotification struct {
	Event      string   `yaml:"event"      json:"event"`
	ToState    string   `yaml:"to_state"   json:"to_state,omitempty"`
	Recipients []map[string]string `yaml:"recipients" json:"recipients"`
	Subject    string   `yaml:"subject"    json:"subject"`
	Message    string   `yaml:"message"    json:"message"`
}

// WorkflowMap maps doctype names to their active workflows.
type WorkflowMap struct {
	workflows map[string]*Workflow // doctype → workflow
}

// NewWorkflowMap creates an empty workflow map.
func NewWorkflowMap() *WorkflowMap {
	return &WorkflowMap{
		workflows: make(map[string]*Workflow),
	}
}

// Register adds a workflow for a doctype.
func (wm *WorkflowMap) Register(wf *Workflow) {
	wm.workflows[wf.DocumentType] = wf
}

// Get returns the active workflow for a doctype, or nil.
func (wm *WorkflowMap) Get(doctype string) *Workflow {
	return wm.workflows[doctype]
}

// Has returns true if the doctype has an active workflow.
func (wm *WorkflowMap) Has(doctype string) bool {
	_, ok := wm.workflows[doctype]
	return ok
}

// Remove removes a workflow from the map.
func (wm *WorkflowMap) Remove(doctype string) {
	delete(wm.workflows, doctype)
}

// GetAvailableTransitions returns transitions available from the current state
// for the given user role. Also evaluates conditions.
func (wm *WorkflowMap) GetAvailableTransitions(doctype, currentState, userRole string, doc *Document) []WorkflowTransition {
	wf := wm.Get(doctype)
	if wf == nil || !wf.IsActive {
		return nil
	}

	var available []WorkflowTransition
	for _, t := range wf.Transitions {
		if t.From != currentState {
			continue
		}
		// Check role.
		if t.Allowed != "" && !roleMatches(t.Allowed, userRole) {
			continue
		}
		// Check condition.
		if t.Condition != "" {
			if !evaluateCondition(t.Condition, doc, nil) {
				continue
			}
		}
		available = append(available, t)
	}
	return available
}

// GetTransition returns the transition with the given action name from the current state.
func (wm *WorkflowMap) GetTransition(doctype, currentState, action string) *WorkflowTransition {
	wf := wm.Get(doctype)
	if wf == nil {
		return nil
	}
	for _, t := range wf.Transitions {
		if t.From == currentState && t.Action == action {
			return &t
		}
	}
	return nil
}

// ApplyTransition validates and applies a workflow transition.
// Returns the new state, new doc_status, and any error.
func (wm *WorkflowMap) ApplyTransition(doctype, currentState, action, userRole string, doc *Document) (string, int, error) {
	wf := wm.Get(doctype)
	if wf == nil {
		return "", 0, fmt.Errorf("no workflow defined for doctype %s", doctype)
	}

	t := wm.GetTransition(doctype, currentState, action)
	if t == nil {
		// Try matching by action only (for case-insensitive or partial matches).
		for _, trans := range wf.Transitions {
			if trans.From == currentState && strings.EqualFold(trans.Action, action) {
				t = &trans
				break
			}
		}
		if t == nil {
			return "", 0, fmt.Errorf("transition %q not available from state %q", action, currentState)
		}
	}

	// Check role.
	if t.Allowed != "" && !roleMatches(t.Allowed, userRole) {
		return "", 0, fmt.Errorf("user role %q is not allowed to perform %q", userRole, action)
	}

	// Check condition.
	if t.Condition != "" {
		if !evaluateCondition(t.Condition, doc, nil) {
			return "", 0, fmt.Errorf("condition not met for transition %q: %s", action, t.Condition)
		}
	}

	// Check required fields.
	for _, fieldName := range t.RequireFields {
		if isNilOrEmpty(doc.Get(fieldName)) {
			return "", 0, fmt.Errorf("field %q is required before %q", fieldName, action)
		}
	}

	// Find the target state's doc_status.
	targetState := wf.getState(t.To)
	if targetState == nil {
		return "", 0, fmt.Errorf("target state %q not found in workflow", t.To)
	}

	return t.To, targetState.DocStatus, nil
}

func (wf *Workflow) getState(name string) *WorkflowState {
	for _, s := range wf.States {
		if s.State == name {
			return &s
		}
	}
	return nil
}

// roleMatches checks if userRole matches the allowed role specification.
// allowed can be a single role or comma-separated list.
func roleMatches(allowed, userRole string) bool {
	if allowed == "" {
		return true
	}
	roles := strings.Split(allowed, ",")
	for _, r := range roles {
		if strings.TrimSpace(r) == userRole {
			return true
		}
	}
	// Administrator bypasses role checks.
	if userRole == AdminRole {
		return true
	}
	return false
}

// ParseWorkflowFile parses a workflow YAML file.
func ParseWorkflowFile(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file: %w", err)
	}
	wf := &Workflow{}
	if err := yaml.Unmarshal(data, wf); err != nil {
		return nil, fmt.Errorf("parsing workflow: %w", err)
	}
	if wf.Name == "" {
		return nil, fmt.Errorf("workflow has no name")
	}
	if wf.DocumentType == "" {
		return nil, fmt.Errorf("workflow %s has no document_type", wf.Name)
	}
	if wf.WorkflowStateField == "" {
		wf.WorkflowStateField = "status"
	}
	return wf, nil
}

// ParseWorkflowDirectory looks for workflow YAML files in a directory.
func ParseWorkflowDirectory(path string) ([]*Workflow, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, nil // No workflows is fine.
	}

	var workflows []*Workflow
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		// Skip known non-workflow files.
		base := entry.Name()
		if base == "roles.yaml" || base == "permissions.yaml" || base == "app.yaml" || base == "scheduler.yaml" {
			continue
		}

		wf, err := ParseWorkflowFile(path + "/" + entry.Name())
		if err != nil {
			// Try to parse as a DocType instead; skip if it's not a workflow.
			if strings.Contains(err.Error(), "has no document_type") {
				continue
			}
			return nil, err
		}
		// Only add if it looks like a workflow (has states).
		if len(wf.States) > 0 {
			workflows = append(workflows, wf)
		}
	}

	return workflows, nil
}
