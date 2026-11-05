package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/expression"
)

const (
	maxDefinitionBytes = 1 << 20
	maxDefinitionSteps = 100
)

var workflowConditionField = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// workflowOwnerModuleID matches contract module IDs such as "finance.ar".
var workflowOwnerModuleID = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)

// ConditionExpression turns the editor's typed field condition into the
// deterministic expression understood by the workflow evaluator.
func ConditionExpression(condition *contract.ConditionSpec) (string, error) {
	if condition == nil {
		return "", fmt.Errorf("condition is required")
	}
	if condition.Field == nil {
		return condition.Expression, nil
	}
	f := condition.Field
	if !workflowConditionField.MatchString(f.Field) {
		return "", fmt.Errorf("condition field is invalid")
	}
	if f.Operator == "is_empty" || f.Operator == "is_not_empty" {
		if len(f.Value) != 0 {
			return "", fmt.Errorf("empty checks do not accept a comparison value")
		}
		op := "="
		if f.Operator == "is_not_empty" {
			op = "!="
		}
		return "doc." + f.Field + " " + op + ` ""`, nil
	}
	operator := map[string]string{"equals": "=", "not_equals": "!=", "greater_than": ">", "at_least": ">=", "less_than": "<", "at_most": "<="}[f.Operator]
	if operator == "" {
		return "", fmt.Errorf("condition operator is unsupported")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(f.Value))
	decoder.UseNumber()
	if len(f.Value) == 0 || decoder.Decode(&value) != nil {
		return "", fmt.Errorf("condition comparison value must be a JSON scalar")
	}
	var literal string
	switch typed := value.(type) {
	case string:
		encoded, _ := json.Marshal(typed)
		literal = string(encoded)
	case json.Number:
		if _, err := strconv.ParseFloat(string(typed), 64); err != nil {
			return "", fmt.Errorf("condition number is invalid")
		}
		literal = string(typed)
	case bool:
		literal = strconv.FormatBool(typed)
	default:
		return "", fmt.Errorf("condition comparison value must be text, number, or true/false")
	}
	return "doc." + f.Field + " " + operator + " " + literal, nil
}

type RecordTriggerSchemaRegistry interface {
	RecordTableExists(string) bool
	RecordFieldExists(string, string) bool
}

type RecordFieldTypeRegistry interface {
	RecordFieldExists(string, string) bool
	RecordFieldType(string, string) (string, bool)
}

// ValidateRecordActionFields binds common registered action inputs to the
// selected table. This keeps notification recipients and typed form controls
// verifiable before a definition is published.
func ValidateRecordActionFields(definition contract.WorkflowDefinition, schemas RecordFieldTypeRegistry) []contract.ValidationIssue {
	if definition.Trigger == nil || schemas == nil {
		return nil
	}
	issues := make([]contract.ValidationIssue, 0)
	for i, step := range definition.Steps {
		if step.Action == nil {
			continue
		}
		if step.Action.Name == "record.update_status" {
			var config StatusActionConfig
			if json.Unmarshal(step.Action.Input, &config) != nil {
				continue // registered schema validation reports malformed JSON.
			}
			fieldPath := fmt.Sprintf("steps[%d].action.input.state_field", i)
			fieldType, found := schemas.RecordFieldType(definition.Trigger.Table, config.StateField)
			if !found {
				issues = append(issues, contract.ValidationIssue{Path: fieldPath, Code: "unknown_field", Detail: "status field does not exist on the selected table"})
			} else if fieldType != "Select" {
				issues = append(issues, contract.ValidationIssue{Path: fieldPath, Code: "not_select", Detail: "status field must be a Select field"})
			} else if options, ok := schemas.(interface {
				RecordFieldOptions(string, string) (string, bool)
			}); ok {
				if raw, exists := options.RecordFieldOptions(definition.Trigger.Table, config.StateField); exists {
					values := map[string]struct{}{}
					for _, option := range strings.Split(raw, "\n") {
						if value := strings.TrimSpace(option); value != "" {
							values[value] = struct{}{}
						}
					}
					for path, value := range map[string]string{"initial_state": config.InitialState, "from": config.From, "to": config.To} {
						if _, exists := values[value]; !exists {
							issues = append(issues, contract.ValidationIssue{Path: fmt.Sprintf("steps[%d].action.input.%s", i, path), Code: "unknown_option", Detail: "status value is not available on the selected field"})
						}
					}
				}
			}
			for j, field := range config.RequireFields {
				if !schemas.RecordFieldExists(definition.Trigger.Table, field) {
					issues = append(issues, contract.ValidationIssue{Path: fmt.Sprintf("steps[%d].action.input.required_fields[%d]", i, j), Code: "unknown_field", Detail: "required field does not exist on the selected table"})
				}
			}
			continue
		}
		if step.Action.Name != "email.send" && step.Action.Name != "notification.send" {
			continue
		}
		var input map[string]json.RawMessage
		if json.Unmarshal(step.Action.Input, &input) != nil {
			continue // the action schema validator reports malformed input.
		}
		fields := make([]string, 0)
		needsEmail := false
		if step.Action.Name == "email.send" {
			var field string
			_ = json.Unmarshal(input["recipient_field"], &field)
			fields = append(fields, field)
			needsEmail = true
		} else {
			var config struct {
				Channel         string   `json:"channel"`
				RecipientFields []string `json:"recipient_fields"`
			}
			_ = json.Unmarshal(step.Action.Input, &config)
			fields = append(fields, config.RecipientFields...)
			needsEmail = config.Channel == "email" || config.Channel == "both"
		}
		for j, field := range fields {
			fieldType, found := schemas.RecordFieldType(definition.Trigger.Table, field)
			path := fmt.Sprintf("steps[%d].action.input.recipient_fields[%d]", i, j)
			if step.Action.Name == "email.send" {
				path = fmt.Sprintf("steps[%d].action.input.recipient_field", i)
			}
			if !found {
				issues = append(issues, contract.ValidationIssue{Path: path, Code: "unknown_field", Detail: "recipient field does not exist on the selected table"})
			} else if needsEmail && fieldType != "Email" {
				issues = append(issues, contract.ValidationIssue{Path: path, Code: "not_email", Detail: "email delivery requires an Email field"})
			}
		}
	}
	return issues
}

func ValidateRecordTriggerSchema(trigger *contract.RecordTrigger, schemas RecordTriggerSchemaRegistry) []contract.ValidationIssue {
	if trigger == nil || schemas == nil {
		return nil
	}
	issues := make([]contract.ValidationIssue, 0)
	if !schemas.RecordTableExists(trigger.Table) {
		issues = append(issues, contract.ValidationIssue{Path: "trigger.table", Code: "unknown_table", Detail: "selected table does not exist"})
		return issues
	}
	for i, field := range trigger.WatchedFields {
		if !schemas.RecordFieldExists(trigger.Table, field) {
			issues = append(issues, contract.ValidationIssue{Path: fmt.Sprintf("trigger.watched_fields[%d]", i), Code: "unknown_field", Detail: "watched field does not exist on the selected table"})
		}
	}
	return issues
}

// DecodeWorkflowDefinition applies the same size and unknown-field boundary
// used by API handlers before a typed definition reaches validation.
func DecodeWorkflowDefinition(data []byte) (contract.WorkflowDefinition, error) {
	var definition contract.WorkflowDefinition
	if len(data) == 0 || len(data) > maxDefinitionBytes {
		return definition, fmt.Errorf("workflow definition must be between 1 byte and 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&definition); err != nil {
		return contract.WorkflowDefinition{}, fmt.Errorf("decode workflow definition: %w", err)
	}
	if err := decoder.Decode(new(any)); err == nil {
		return contract.WorkflowDefinition{}, fmt.Errorf("decode workflow definition: trailing JSON value")
	} else if err != io.EOF {
		return contract.WorkflowDefinition{}, fmt.Errorf("decode workflow definition trailing data: %w", err)
	}
	return definition, nil
}

// ValidateDefinition rejects malformed or unsafe graphs before they can be
// published. It does not mutate the definition or resolve runtime effects.
func ValidateDefinition(def contract.WorkflowDefinition, catalog *Catalog, schemas PayloadSchemaRegistry, waits WaitFieldRegistry) contract.ValidationResult {
	issues := make([]contract.ValidationIssue, 0)
	add := func(path, code, detail string) {
		if len(issues) < 100 {
			issues = append(issues, contract.ValidationIssue{Path: path, Code: code, Detail: detail})
		}
	}
	encoded, err := json.Marshal(def)
	if err != nil {
		add("$", "invalid_json", err.Error())
		return contract.ValidationResult{Issues: issues}
	}
	if len(encoded) > maxDefinitionBytes {
		add("$", "too_large", "workflow definition exceeds 1 MiB")
	}
	if def.ID == "" || def.Version == 0 {
		add("$", "identity_required", "definition ID and positive version are required")
	} else if _, err := contract.ParseDefinitionID(string(def.ID)); err != nil {
		add("id", "invalid_id", err.Error())
	}
	if def.OwnerModuleID != "" && !workflowOwnerModuleID.MatchString(string(def.OwnerModuleID)) {
		add("owner_module_id", "invalid_module_owner", "workflow owner module ID is invalid")
	}
	if len(def.Steps) == 0 || len(def.Steps) > maxDefinitionSteps {
		add("steps", "step_count", "definition must contain between 1 and 100 steps")
	}
	if def.Trigger != nil {
		if def.Trigger.Kind != "record.created" && def.Trigger.Kind != "record.updated" {
			add("trigger.kind", "unsupported_trigger", "trigger must be record.created or record.updated")
		}
		if strings.TrimSpace(def.Trigger.Table) == "" || len(def.Trigger.Table) > 160 || strings.TrimSpace(def.Trigger.Table) != def.Trigger.Table {
			add("trigger.table", "table_required", "trigger table must be a canonical table name")
		}
		seen := make(map[string]struct{}, len(def.Trigger.WatchedFields))
		for i, field := range def.Trigger.WatchedFields {
			if strings.TrimSpace(field) == "" || len(field) > 160 || strings.TrimSpace(field) != field {
				add(fmt.Sprintf("trigger.watched_fields[%d]", i), "invalid_field", "watched field name is invalid")
			}
			if _, exists := seen[field]; exists {
				add(fmt.Sprintf("trigger.watched_fields[%d]", i), "duplicate_field", "watched fields must be unique")
			}
			seen[field] = struct{}{}
		}
		if def.Trigger.Kind == "record.created" && len(def.Trigger.WatchedFields) > 0 {
			add("trigger.watched_fields", "unexpected_fields", "watched fields apply only to record.updated triggers")
		}
	}
	byID := make(map[contract.StepID]contract.StepDefinition, len(def.Steps))
	for i, step := range def.Steps {
		path := fmt.Sprintf("steps[%d]", i)
		if step.ID == "" {
			add(path+".id", "required", "step ID is required")
		} else if _, err := contract.ParseStepID(string(step.ID)); err != nil {
			add(path+".id", "invalid_id", err.Error())
		} else if _, exists := byID[step.ID]; exists {
			add(path+".id", "duplicate", "step IDs must be unique")
		} else {
			byID[step.ID] = step
		}
		count := 0
		for _, present := range []bool{step.Action != nil, step.Condition != nil, step.Human != nil, step.Wait != nil, step.End != nil} {
			if present {
				count++
			}
		}
		if count != 1 {
			add(path, "union", "exactly one step variant must be present")
		}
		var expected contract.StepKind
		switch step.Kind {
		case contract.StepAction:
			expected = contract.StepAction
			if step.Action == nil {
				add(path, "union", "action step requires action payload")
			}
		case contract.StepCondition:
			expected = contract.StepCondition
			if step.Condition == nil {
				add(path, "union", "condition step requires condition payload")
			}
		case contract.StepHuman:
			expected = contract.StepHuman
			if step.Human == nil {
				add(path, "union", "human step requires human payload")
			}
		case contract.StepWait:
			expected = contract.StepWait
			if step.Wait == nil {
				add(path, "union", "wait step requires wait payload")
			}
		case contract.StepEnd:
			expected = contract.StepEnd
			if step.End == nil {
				add(path, "union", "end step requires end payload")
			}
		default:
			add(path+".kind", "unknown_kind", "step kind is unsupported")
		}
		_ = expected
	}
	if len(def.Steps) == 0 {
		return contract.ValidationResult{Issues: issues}
	}
	if _, ok := byID[def.Entry]; !ok {
		add("entry", "missing_step", "entry must name a defined step")
	}

	adj := make(map[contract.StepID][]contract.StepID, len(byID))
	for i, step := range def.Steps {
		path := fmt.Sprintf("steps[%d]", i)
		addEdge := func(field string, to contract.StepID, required bool) {
			if to == "" {
				if required {
					add(path+"."+field, "destination_required", "destination is required")
				}
				return
			}
			if _, ok := byID[to]; !ok {
				add(path+"."+field, "missing_destination", "destination does not exist")
				return
			}
			adj[step.ID] = append(adj[step.ID], to)
		}
		switch step.Kind {
		case contract.StepAction:
			if step.Action == nil {
				continue
			}
			action := step.Action
			addEdge("action.next", action.Next, true)
			if action.Name == "" || action.Version == 0 {
				add(path+".action", "action_key", "action name and positive version are required")
			}
			if action.InputSchema == "" {
				add(path+".action.input_schema", "schema_required", "input schema reference is required")
			}
			if len(action.Input) > 65536 {
				add(path+".action.input", "too_large", "action input exceeds 64 KiB")
			}
			if catalog == nil {
				add(path+".action", "catalog_required", "action catalog is unavailable")
				continue
			}
			reg, resolveErr := catalog.Resolve(ActionKey{Name: action.Name, Version: action.Version})
			if resolveErr != nil {
				add(path+".action", "unknown_action", resolveErr.Error())
				continue
			}
			if action.InputSchema != reg.InputSchema.ID {
				add(path+".action.input_schema", "schema_mismatch", "action input schema does not match registered action")
			}
			if schemas == nil {
				add(path+".action.input", "schema_registry_required", "payload schema registry is unavailable")
			} else if !schemas.Registered(reg.InputSchema) {
				add(path+".action.input", "unknown_schema", "registered action input schema is unavailable")
			} else if err := schemas.Validate(reg.InputSchema, action.Input); err != nil {
				add(path+".action.input", "invalid_input", err.Error())
			}
		case contract.StepCondition:
			if step.Condition == nil {
				continue
			}
			cond := step.Condition
			addEdge("condition.if_true", cond.IfTrue, true)
			addEdge("condition.if_false", cond.IfFalse, true)
			expressionText, conditionErr := ConditionExpression(cond)
			if conditionErr != nil {
				add(path+".condition.field", "invalid_typed_condition", conditionErr.Error())
			} else if cond.Field != nil && cond.Expression != "" {
				add(path+".condition", "condition_union", "choose a typed field condition or an expression, not both")
			} else if _, err := expression.Parse(expressionText, 100, 64); err != nil {
				add(path+".condition.expression", "invalid_expression", err.Error())
			}
		case contract.StepHuman:
			if step.Human == nil {
				continue
			}
			human := step.Human
			allowed := allowedResponses(human.Request.Kind)
			if human.Request.Subject != contract.SubjectInstance {
				add(path+".human.request.subject", "subject_binding", "human request must bind to the workflow subject")
			}
			if human.Request.Title == "" || len([]rune(human.Request.Title)) > 160 || len(human.Request.Message) > 8192 || human.Request.DueAfterSeconds == 0 || human.Request.DueAfterSeconds > 2678400 {
				add(path+".human.request", "template_bounds", "title, message, or deadline is outside its allowed bounds")
			}
			if len(allowed) == 0 {
				add(path+".human.request.kind", "unknown_request_kind", "human request kind is unsupported")
			}
			if human.Request.Kind == contract.RequestApproval && (human.Request.PolicyID == "" || human.Request.Assignment != nil) {
				add(path+".human.request", "governed_approval", "approval templates require a server policy and cannot supply assignments")
			}
			if human.Request.Kind == contract.RequestInformation && human.Request.InformationSchema == nil {
				add(path+".human.request.information_schema", "schema_required", "information requests require a registered response schema")
			}
			if human.Request.InformationSchema != nil && (schemas == nil || !schemas.Registered(*human.Request.InformationSchema)) {
				add(path+".human.request.information_schema", "unknown_schema", "response schema is not registered")
			}
			if human.Request.Kind != contract.RequestInformation && human.Request.InformationSchema != nil {
				add(path+".human.request.information_schema", "unexpected_schema", "only information requests may declare a response schema")
			}
			if human.Request.Assignment != nil {
				validateTemplateAssignment(human.Request.Assignment, path+".human.request.assignment", add)
			}
			if len(human.Outcomes) != len(allowed) {
				add(path+".human.outcomes", "outcome_coverage", "human outcomes must cover every response allowed by request kind")
			}
			keys := make([]string, 0, len(human.Outcomes))
			for response := range human.Outcomes {
				keys = append(keys, string(response))
			}
			sort.Strings(keys)
			for _, responseText := range keys {
				response := contract.ResponseKind(responseText)
				to := human.Outcomes[response]
				if _, ok := allowed[response]; !ok {
					add(path+".human.outcomes."+responseText, "response_not_allowed", "response is not allowed for request kind")
				}
				addEdge("human.outcomes."+responseText, to, true)
			}
			addEdge("human.on_expired", human.OnExpired, true)
			addEdge("human.on_cancelled", human.OnCancelled, true)
			addEdge("human.on_stale", human.OnStale, true)
		case contract.StepWait:
			if step.Wait == nil {
				continue
			}
			wait := step.Wait
			if wait.TimeoutSeconds == 0 || wait.TimeoutSeconds > 2678400 {
				add(path+".wait.timeout_seconds", "timeout_required", "wait timeout must be between 1 second and 31 days")
			}
			addEdge("wait.on_timeout", wait.OnTimeout, true)
			if wait.EventType == "" {
				if wait.OnEvent != "" || wait.CorrelationField != "" {
					add(path+".wait", "event_registration", "event routing requires a registered event type")
				}
			} else {
				if wait.OnEvent == "" {
					add(path+".wait.on_event", "destination_required", "event destination is required")
				} else {
					addEdge("wait.on_event", wait.OnEvent, true)
				}
				if wait.CorrelationField == "" || waits == nil || !waits.RegisteredCorrelationField(wait.EventType, wait.CorrelationField) {
					add(path+".wait.correlation_field", "unregistered_correlation", "event correlation field must be registered")
				}
			}
		case contract.StepEnd:
			if step.End == nil {
				continue
			}
			if step.End.Outcome == "" {
				add(path+".end.outcome", "outcome_required", "end outcome is required")
			}
		}
	}
	// DFS rejects cycles and identifies unreachable nodes with bounded work.
	color := make(map[contract.StepID]uint8, len(byID))
	reachable := make(map[contract.StepID]bool, len(byID))
	var visit func(contract.StepID)
	visit = func(id contract.StepID) {
		if color[id] == 1 {
			add("steps", "cycle", "workflow graph contains a cycle")
			return
		}
		if color[id] == 2 {
			return
		}
		color[id] = 1
		reachable[id] = true
		for _, next := range adj[id] {
			visit(next)
		}
		color[id] = 2
	}
	if _, ok := byID[def.Entry]; ok {
		visit(def.Entry)
	}
	for id := range byID {
		if !reachable[id] {
			add("steps", "unreachable", "step "+string(id)+" is unreachable from entry")
		}
	}
	return contract.ValidationResult{Valid: len(issues) == 0, Issues: issues}
}

func allowedResponses(kind contract.RequestKind) map[contract.ResponseKind]struct{} {
	responses := map[contract.ResponseKind]struct{}{}
	switch kind {
	case contract.RequestApproval:
		responses[contract.ResponseApprove] = struct{}{}
		responses[contract.ResponseReject] = struct{}{}
		responses[contract.ResponseReturn] = struct{}{}
	case contract.RequestReview, contract.RequestTask:
		responses[contract.ResponseComplete] = struct{}{}
		responses[contract.ResponseReturn] = struct{}{}
	case contract.RequestInformation:
		responses[contract.ResponseProvide] = struct{}{}
	case contract.RequestAcknowledgment:
		responses[contract.ResponseAcknowledge] = struct{}{}
	}
	return responses
}

func validateTemplateAssignment(assignment *contract.AssignmentSpec, path string, add func(string, string, string)) {
	if assignment.Mode != contract.AssignmentAny && assignment.Mode != contract.AssignmentAll && assignment.Mode != contract.AssignmentSequential {
		add(path+".mode", "invalid_assignment_mode", "assignment mode is unsupported")
	}
	if len(assignment.Assignees) == 0 || len(assignment.Assignees) > 50 {
		add(path+".assignees", "assignment_bounds", "assignment must have between 1 and 50 assignees")
	}
	seen := make(map[contract.Assignee]bool, len(assignment.Assignees))
	for i, assignee := range assignment.Assignees {
		if assignee.ID == "" || (assignee.Kind != contract.AssigneeUser && assignee.Kind != contract.AssigneeRole && assignee.Kind != contract.AssigneeGroup) {
			add(fmt.Sprintf("%s.assignees[%d]", path, i), "invalid_assignee", "assignee kind and ID are required")
			continue
		}
		if seen[assignee] {
			add(fmt.Sprintf("%s.assignees[%d]", path, i), "duplicate_assignee", "assignment targets must be unique")
		}
		seen[assignee] = true
	}
}
