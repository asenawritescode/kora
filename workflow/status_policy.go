package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/expression"
)

// StatusPolicy is assembled from registered record.update_status steps in
// published workflow definitions. It is deliberately not a separate config
// resource: the transition and its execution graph are published together.
type StatusPolicy struct {
	Table        string
	StateField   string
	InitialState string
	Transitions  []StatusTransition
}

type StatusTransition struct {
	Action        string   `json:"transition_action"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	AllowedRoles  []string `json:"allowed_roles,omitempty"`
	RequireFields []string `json:"required_fields,omitempty"`
	Condition     string   `json:"condition,omitempty"`
	DocStatus     int      `json:"doc_status,omitempty"`
}

// StatusActionConfig is the complete policy for one registered
// record.update_status step. Publishing the workflow publishes this policy and
// its executable transition together.
type StatusActionConfig struct {
	StateField   string `json:"state_field"`
	InitialState string `json:"initial_state"`
	StatusTransition
}

func ValidateStatusTransition(transition StatusTransition) []contract.ValidationIssue {
	issues := make([]contract.ValidationIssue, 0)
	if strings.TrimSpace(transition.Action) == "" || strings.TrimSpace(transition.From) == "" || strings.TrimSpace(transition.To) == "" {
		issues = append(issues, contract.ValidationIssue{Path: "action.input", Code: "required", Detail: "transition action, from state and to state are required"})
	}
	if transition.Condition != "" {
		if _, err := expression.Parse(transition.Condition, 100, 64); err != nil {
			issues = append(issues, contract.ValidationIssue{Path: "action.input.condition", Code: "invalid_expression", Detail: err.Error()})
		}
	}
	return issues
}

// ApplyStatusTransition evaluates the selected transition against a record
// snapshot. The caller persists the returned status in its current SQL tx.
func ApplyStatusTransition(policy *StatusPolicy, currentState, action string, roles []string, doc *doctype.Document) (string, int, error) {
	if policy == nil || doc == nil || action == "" {
		return "", 0, fmt.Errorf("status policy, record and transition action are required")
	}
	var selected *StatusTransition
	for i := range policy.Transitions {
		transition := &policy.Transitions[i]
		if transition.From == currentState && strings.EqualFold(transition.Action, action) {
			if selected != nil {
				return "", 0, fmt.Errorf("transition action %q is ambiguous from state %q", action, currentState)
			}
			selected = transition
		}
	}
	if selected == nil {
		return "", 0, fmt.Errorf("transition %q not available from state %q", action, currentState)
	}
	if len(selected.AllowedRoles) > 0 {
		allowed := false
		for _, actual := range roles {
			for _, required := range selected.AllowedRoles {
				if actual == required || actual == doctype.AdminRole {
					allowed = true
					break
				}
			}
			if allowed {
				break
			}
		}
		if !allowed {
			return "", 0, fmt.Errorf("actor roles are not allowed to perform %q", action)
		}
	}
	for _, field := range selected.RequireFields {
		if emptyWorkflowValue(doc.Get(field)) {
			return "", 0, fmt.Errorf("field %q is required before %q", field, action)
		}
	}
	if selected.Condition != "" {
		parsed, err := expression.Parse(selected.Condition, 100, 64)
		if err != nil {
			return "", 0, fmt.Errorf("parse transition condition: %w", err)
		}
		value, err := expression.NewTreeEvaluator().Eval(context.Background(), parsed.AST, statusExpressionScope{doc: doc})
		if err != nil {
			return "", 0, fmt.Errorf("evaluate transition condition: %w", err)
		}
		if value.Kind != expression.ValueBool || !value.Bool {
			return "", 0, fmt.Errorf("transition condition was not met")
		}
	}
	return selected.To, selected.DocStatus, nil
}

func emptyWorkflowValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.String, reflect.Array, reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	}
	return false
}

type statusExpressionScope struct{ doc *doctype.Document }

func (s statusExpressionScope) Lookup(path string) (expression.Value, bool) {
	if s.doc == nil {
		return expression.NullValue(), false
	}
	name := strings.TrimPrefix(path, "doc.")
	if name == path {
		return expression.NullValue(), false
	}
	value := s.doc.Get(name)
	if value == nil {
		return expression.NullValue(), true
	}
	switch typed := value.(type) {
	case string:
		return expression.StringValue(typed), true
	case bool:
		return expression.BoolValue(typed), true
	case int:
		return expression.NumberValue(float64(typed)), true
	case int64:
		return expression.NumberValue(float64(typed)), true
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return expression.NumberValue(f), true
		}
		return expression.StringValue(string(typed)), true
	case float64:
		return expression.NumberValue(typed), true
	case []byte:
		if number, err := strconv.ParseFloat(string(typed), 64); err == nil {
			return expression.NumberValue(number), true
		}
		return expression.StringValue(string(typed)), true
	default:
		return expression.StringValue(fmt.Sprint(typed)), true
	}
}

func (statusExpressionScope) Aggregate(string, string, string) (expression.Value, error) {
	return expression.NullValue(), fmt.Errorf("status conditions do not allow aggregates")
}
