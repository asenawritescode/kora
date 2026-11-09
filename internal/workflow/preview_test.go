package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestPreviewRecordDefinitionBranchesWithoutExecutingActions(t *testing.T) {
	definition := contract.WorkflowDefinition{
		ID: "notify-large-orders", Version: 1, Entry: "condition",
		Trigger: &contract.RecordTrigger{Kind: "record.created", Table: "Order"},
		Steps: []contract.StepDefinition{
			{ID: "condition", Kind: contract.StepCondition, Condition: &contract.ConditionSpec{Field: &contract.FieldCondition{Field: "total", Operator: "greater_than", Value: json.RawMessage(`100`)}, IfTrue: "send", IfFalse: "skip"}},
			{ID: "send", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "email.send", Version: 1, Input: json.RawMessage(`{"to":"owner@example.test"}`), Next: "done"}},
			{ID: "skip", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "below_limit"}},
			{ID: "done", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "notified"}},
		},
	}
	result, err := PreviewRecordDefinition(context.Background(), definition, json.RawMessage(`{"total":125}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Matched || result.Outcome != "notified" || len(result.Steps) != 3 {
		t.Fatalf("unexpected preview result: %+v", result)
	}
	if result.Steps[1].Result != "would run email.send (not sent during preview)" {
		t.Fatalf("action was not clearly marked as preview-only: %+v", result.Steps[1])
	}

	result, err = PreviewRecordDefinition(context.Background(), definition, json.RawMessage(`{"total":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "below_limit" || len(result.Steps) != 2 {
		t.Fatalf("false branch was not selected: %+v", result)
	}
}

func TestPreviewRecordDefinitionRejectsNonRecordTriggerAndUnsafeSamples(t *testing.T) {
	definition := contract.WorkflowDefinition{Entry: "done", Steps: []contract.StepDefinition{{ID: "done", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}}}}
	if _, err := PreviewRecordDefinition(context.Background(), definition, json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected missing record trigger to be rejected")
	}
	definition.Trigger = &contract.RecordTrigger{Kind: "record.created", Table: "Order"}
	if _, err := PreviewRecordDefinition(context.Background(), definition, json.RawMessage(`[]`)); err == nil {
		t.Fatal("expected non-object sample to be rejected")
	}
}
