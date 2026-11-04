package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/expression"
)

const maxPreviewRecordBytes = 64 << 10

// PreviewStep is a side-effect-free account of what a sample record would do.
type PreviewStep struct {
	StepID string `json:"step_id"`
	Kind   string `json:"kind"`
	Result string `json:"result"`
}

// PreviewResult never represents a live workflow instance. Actions are only
// described; they are not dispatched and records are never mutated.
type PreviewResult struct {
	Matched bool          `json:"matched"`
	Outcome string        `json:"outcome,omitempty"`
	Steps   []PreviewStep `json:"steps"`
}

type previewScope map[string]expression.Value

func (s previewScope) Lookup(path string) (expression.Value, bool) {
	value, ok := s[strings.TrimPrefix(path, "doc.")]
	return value, ok
}
func (previewScope) Aggregate(string, string, string) (expression.Value, error) {
	return expression.Value{}, fmt.Errorf("aggregates are unavailable in record previews")
}

// PreviewRecordDefinition evaluates only the graph and typed conditions. It
// deliberately does not invoke registered actions, providers, approval
// policies, waits, or the normal instance store.
func PreviewRecordDefinition(ctx context.Context, definition contract.WorkflowDefinition, raw json.RawMessage) (PreviewResult, error) {
	if definition.Trigger == nil || (definition.Trigger.Kind != "record.created" && definition.Trigger.Kind != "record.updated") {
		return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "preview requires a record-triggered workflow")
	}
	if len(raw) == 0 || len(raw) > maxPreviewRecordBytes {
		return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "sample record must be a JSON object up to 64 KB")
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil || record == nil {
		return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "sample record must be a JSON object")
	}
	byID := make(map[contract.StepID]contract.StepDefinition, len(definition.Steps))
	for _, step := range definition.Steps {
		byID[step.ID] = step
	}
	scope := previewScope{}
	for field, encoded := range record {
		var value any
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			continue
		}
		switch v := value.(type) {
		case string:
			scope[field] = expression.StringValue(v)
		case json.Number:
			n, err := strconv.ParseFloat(string(v), 64)
			if err == nil {
				scope[field] = expression.NumberValue(n)
			}
		case bool:
			scope[field] = expression.BoolValue(v)
		}
	}
	result := PreviewResult{Matched: true, Steps: make([]PreviewStep, 0)}
	current := definition.Entry
	evaluator := expression.NewTreeEvaluator()
	for hops := 0; hops < len(definition.Steps)+1; hops++ {
		if err := ctx.Err(); err != nil {
			return PreviewResult{}, err
		}
		step, ok := byID[current]
		if !ok {
			return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow preview reached a missing step")
		}
		switch step.Kind {
		case contract.StepCondition:
			if step.Condition == nil {
				return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow condition is incomplete")
			}
			text, err := ConditionExpression(step.Condition)
			if err != nil {
				return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow condition is invalid")
			}
			parsed, err := expression.Parse(text, 100, 64)
			if err != nil {
				return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow condition is invalid")
			}
			value, err := evaluator.Eval(ctx, parsed.AST, scope)
			if err != nil || value.Kind != expression.ValueBool {
				return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "sample record could not evaluate this condition")
			}
			next, outcome := step.Condition.IfFalse, "condition was false"
			if value.Bool {
				next, outcome = step.Condition.IfTrue, "condition was true"
			}
			result.Steps = append(result.Steps, PreviewStep{StepID: string(step.ID), Kind: string(step.Kind), Result: outcome})
			current = next
		case contract.StepAction:
			if step.Action == nil {
				return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow action is incomplete")
			}
			result.Steps = append(result.Steps, PreviewStep{StepID: string(step.ID), Kind: string(step.Kind), Result: "would run " + step.Action.Name + " (not sent during preview)"})
			current = step.Action.Next
		case contract.StepHuman:
			result.Steps = append(result.Steps, PreviewStep{StepID: string(step.ID), Kind: string(step.Kind), Result: "would request a decision (not assigned during preview)"})
			return result, nil
		case contract.StepWait:
			result.Steps = append(result.Steps, PreviewStep{StepID: string(step.ID), Kind: string(step.Kind), Result: "would wait for an event (not scheduled during preview)"})
			return result, nil
		case contract.StepEnd:
			if step.End == nil {
				return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow end step is incomplete")
			}
			result.Steps = append(result.Steps, PreviewStep{StepID: string(step.ID), Kind: string(step.Kind), Result: "flow ends"})
			result.Outcome = step.End.Outcome
			return result, nil
		default:
			return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow contains an unsupported preview step")
		}
	}
	return PreviewResult{}, contract.NewError(contract.CodeValidationFailed, "workflow preview exceeded its step limit")
}
