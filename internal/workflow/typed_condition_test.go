package workflow

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestConditionExpressionBuildsSafeTypedComparisons(t *testing.T) {
	tests := []struct {
		field contract.FieldCondition
		want  string
	}{
		{contract.FieldCondition{Field: "total", Operator: "greater_than", Value: json.RawMessage(`125.5`)}, "doc.total > 125.5"},
		{contract.FieldCondition{Field: "status", Operator: "equals", Value: json.RawMessage(`"Ready\" now"`)}, `doc.status = "Ready\" now"`},
		{contract.FieldCondition{Field: "approved", Operator: "equals", Value: json.RawMessage(`true`)}, "doc.approved = true"},
		{contract.FieldCondition{Field: "owner", Operator: "is_empty"}, `doc.owner = ""`},
	}
	for _, test := range tests {
		got, err := ConditionExpression(&contract.ConditionSpec{Field: &test.field})
		if err != nil {
			t.Fatalf("ConditionExpression(%+v): %v", test.field, err)
		}
		if got != test.want {
			t.Errorf("ConditionExpression(%+v) = %q, want %q", test.field, got, test.want)
		}
	}
}

func TestConditionExpressionRejectsInjectionAndCompoundValues(t *testing.T) {
	for _, field := range []contract.FieldCondition{
		{Field: "total) OR TRUE", Operator: "equals", Value: json.RawMessage(`1`)},
		{Field: "status", Operator: "equals", Value: json.RawMessage(`{"x":1}`)},
		{Field: "status", Operator: "contains", Value: json.RawMessage(`"Ready"`)},
	} {
		if _, err := ConditionExpression(&contract.ConditionSpec{Field: &field}); err == nil {
			t.Errorf("ConditionExpression(%+v) unexpectedly succeeded", field)
		}
	}
}
