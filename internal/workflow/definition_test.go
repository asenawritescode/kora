package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

type testExternalAction struct{}

func (testExternalAction) Execute(context.Context, ActionContext, ActionInput) (ActionResult, error) {
	return ActionResult{}, nil
}

type testSchemas struct{}

func (testSchemas) Registered(ref contract.SchemaRef) bool {
	return ref.ID == "input" || ref.ID == "output"
}

func (testSchemas) Validate(ref contract.SchemaRef, data json.RawMessage) error {
	if ref.ID != "input" || !json.Valid(data) {
		return context.Canceled
	}
	return nil
}

type testWaits struct{}

func (testWaits) RegisteredCorrelationField(event, field string) bool {
	return event == "invoice.updated" && field == "invoice_id"
}

func frozenTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	c := NewCatalog()
	if err := c.Register(ActionRegistration{Key: ActionKey{Name: "invoice.approve", Version: 1}, Class: ActionExternal, InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}, RequiredGrant: testActionGrant(), External: testExternalAction{}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Freeze(); err != nil {
		t.Fatal(err)
	}
	return c
}

func validTestDefinition() contract.WorkflowDefinition {
	return contract.WorkflowDefinition{ID: "approval", Version: 1, Entry: "approve", Steps: []contract.StepDefinition{
		{ID: "approve", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "invoice.approve", Version: 1, InputSchema: "input", Input: json.RawMessage(`{}`), Next: "done"}},
		{ID: "done", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "success"}},
	}}
}

func TestValidateDefinitionAcceptsRegisteredActionAndBoundedGraph(t *testing.T) {
	result := ValidateDefinition(validTestDefinition(), frozenTestCatalog(t), testSchemas{}, testWaits{})
	if !result.Valid || len(result.Issues) != 0 {
		t.Fatalf("unexpected validation result: %+v", result)
	}
}

func TestValidateDefinitionRejectsUnknownActionMissingEdgeCycleAndUnreachable(t *testing.T) {
	t.Run("unknown action", func(t *testing.T) {
		d := validTestDefinition()
		d.Steps[0].Action.Name = "shell"
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "unknown_action") {
			t.Fatalf("expected unknown action issue: %+v", result)
		}
	})
	t.Run("missing destination", func(t *testing.T) {
		d := validTestDefinition()
		d.Steps[0].Action.Next = "absent"
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "missing_destination") {
			t.Fatalf("expected missing destination issue: %+v", result)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		d := validTestDefinition()
		d.Steps[0].Action.Next = "approve"
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "cycle") {
			t.Fatalf("expected cycle issue: %+v", result)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		d := validTestDefinition()
		d.Steps = append(d.Steps, contract.StepDefinition{ID: "unused", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "unused"}})
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "unreachable") {
			t.Fatalf("expected unreachable issue: %+v", result)
		}
	})
}

func TestValidateDefinitionRejectsMalformedUnionUnsafeExpressionAndUnregisteredWait(t *testing.T) {
	t.Run("malformed union", func(t *testing.T) {
		d := validTestDefinition()
		d.Steps[0].End = &contract.EndSpec{Outcome: "no"}
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "union") {
			t.Fatalf("expected union issue: %+v", result)
		}
	})
	t.Run("forbidden expression function", func(t *testing.T) {
		d := contract.WorkflowDefinition{ID: "condition", Version: 1, Entry: "branch", Steps: []contract.StepDefinition{{ID: "branch", Kind: contract.StepCondition, Condition: &contract.ConditionSpec{Expression: "SUM(invoice.total) > 0", IfTrue: "yes", IfFalse: "no"}}, {ID: "yes", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "yes"}}, {ID: "no", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "no"}}}}
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "invalid_expression") {
			t.Fatalf("expected expression issue: %+v", result)
		}
	})
	t.Run("unregistered correlation", func(t *testing.T) {
		d := contract.WorkflowDefinition{ID: "wait", Version: 1, Entry: "wait", Steps: []contract.StepDefinition{{ID: "wait", Kind: contract.StepWait, Wait: &contract.WaitSpec{EventType: "invoice.updated", CorrelationField: "raw_sql", TimeoutSeconds: 1, OnEvent: "done", OnTimeout: "done"}}, {ID: "done", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "done"}}}}
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "unregistered_correlation") {
			t.Fatalf("expected correlation issue: %+v", result)
		}
	})
	t.Run("bounds", func(t *testing.T) {
		d := validTestDefinition()
		d.Steps[0].Action.Input = json.RawMessage(`"` + strings.Repeat("x", 65536) + `"`)
		result := ValidateDefinition(d, frozenTestCatalog(t), testSchemas{}, testWaits{})
		if result.Valid || !hasIssue(result, "too_large") {
			t.Fatalf("expected bounded input issue: %+v", result)
		}
	})
}

func TestCatalogFreezesAndRejectsAdapterMismatch(t *testing.T) {
	c := NewCatalog()
	registration := ActionRegistration{Key: ActionKey{Name: "x", Version: 1}, Class: ActionExternal, InputSchema: contract.SchemaRef{ID: "i", Version: 1}, OutputSchema: contract.SchemaRef{ID: "o", Version: 1}, RequiredGrant: testActionGrant()}
	if err := c.Register(registration); err == nil {
		t.Fatal("expected class/adapter mismatch")
	}
	registration.External = testExternalAction{}
	if err := c.Register(registration); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve(registration.Key); err == nil {
		t.Fatal("resolve before freeze must fail")
	}
	if err := c.Freeze(); err != nil {
		t.Fatal(err)
	}
	if err := c.Register(registration); err == nil {
		t.Fatal("registration after freeze must fail")
	}
	if _, err := c.Resolve(registration.Key); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRejectsActionWithoutExactGrant(t *testing.T) {
	catalog := NewCatalog()
	err := catalog.Register(ActionRegistration{
		Key: ActionKey{Name: "x", Version: 1}, Class: ActionExternal,
		InputSchema: contract.SchemaRef{ID: "i", Version: 1}, OutputSchema: contract.SchemaRef{ID: "o", Version: 1},
		External: testExternalAction{},
	})
	if err == nil {
		t.Fatal("registered an action without an exact trusted grant")
	}
}

func TestDecodeWorkflowDefinitionRejectsUnknownFieldsTrailingJSONAndOversize(t *testing.T) {
	if _, err := DecodeWorkflowDefinition([]byte(`{"id":"x","version":1,"entry":"end","steps":[],"shell":"rm -rf"}`)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	if _, err := DecodeWorkflowDefinition([]byte(`{"id":"x","version":1,"entry":"end","steps":[]} {}`)); err == nil {
		t.Fatal("trailing value was accepted")
	}
	if _, err := DecodeWorkflowDefinition([]byte(strings.Repeat(" ", maxDefinitionBytes+1))); err == nil {
		t.Fatal("oversized body was accepted")
	}
}

func hasIssue(result contract.ValidationResult, code string) bool {
	for _, issue := range result.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func TestValidateDefinitionChecksOwnerModuleID(t *testing.T) {
	cases := []struct {
		owner contract.ModuleID
		valid bool
	}{
		{owner: "", valid: true},
		{owner: "finance.ar", valid: true},
		{owner: "pos.till-sessions", valid: true},
		{owner: "Finance.AR", valid: false},
		{owner: "finance", valid: false},
		{owner: "finance..ar", valid: false},
	}
	for _, tc := range cases {
		def := validTestDefinition()
		def.OwnerModuleID = tc.owner
		result := ValidateDefinition(def, frozenTestCatalog(t), testSchemas{}, testWaits{})
		flagged := false
		for _, issue := range result.Issues {
			if issue.Path == "owner_module_id" && issue.Code == "invalid_module_owner" {
				flagged = true
			}
		}
		if flagged == tc.valid {
			t.Fatalf("owner %q flagged=%v, want valid=%v (issues %+v)", tc.owner, flagged, tc.valid, result.Issues)
		}
	}
}
