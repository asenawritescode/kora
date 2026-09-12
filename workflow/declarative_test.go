package workflow

import (
	"context"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

type declarativeExecutor struct{ capabilities []string }

type declarativeRuleEvaluator struct{ result bool }

func (e declarativeRuleEvaluator) Evaluate(context.Context, string, any) (bool, error) {
	return e.result, nil
}

func (e *declarativeExecutor) Execute(_ context.Context, capability string, _ contract.ActorContext, _ any) error {
	e.capabilities = append(e.capabilities, capability)
	return nil
}

func TestDeclarativeWorkflowParsesAndRunsThroughCapabilityExecutor(t *testing.T) {
	wf, err := ParseDeclarativeWorkflow([]byte(`
name: purchasing
namespace: package
version: 1
kind: workflow
trigger: package.low_stock.detected
steps:
  - id: recommend
    capability: package.create_request
    actor: human_or_agent
  - id: approve
    capability: package.approve_request
    actor: human
`))
	if err != nil {
		t.Fatal(err)
	}
	executor := &declarativeExecutor{}
	steps, err := RunDeclarativeWorkflow(context.Background(), wf, contract.ActorContext{PrincipalID: "manager", PrincipalType: contract.PrincipalHuman}, map[string]any{"item": "A"}, executor)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || len(executor.capabilities) != 2 {
		t.Fatalf("steps=%v capabilities=%v", steps, executor.capabilities)
	}
}

func TestDeclarativeWorkflowBlocksNonHumanApprovalStep(t *testing.T) {
	wf, err := ParseDeclarativeWorkflow([]byte(`
name: approval
namespace: package
version: 1
kind: workflow
trigger: package.requested
steps:
  - id: approve
    capability: package.approve_request
    actor: human
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunDeclarativeWorkflow(context.Background(), wf, contract.ActorContext{PrincipalID: "agent", PrincipalType: contract.PrincipalAgent}, nil, &declarativeExecutor{})
	if err == nil {
		t.Fatal("non-human actor passed human approval gate")
	}
}

func TestDeclarativeRuleRejectsUnknownFields(t *testing.T) {
	_, err := ParseDeclarativeRule([]byte("name: x\nnamespace: package\nversion: 1\nkind: rule\nunknown: true\n"))
	if err == nil {
		t.Fatal("rule parser accepted unknown field")
	}
}

func TestDeclarativeRuleUsesSharedEvaluatorAndEventGate(t *testing.T) {
	rule, err := ParseDeclarativeRule([]byte(`
name: low_stock
namespace: package
version: 1
kind: rule
when:
  event: package.stock.changed
evaluate:
  entity: Balance
  predicate: quantity < reorder_level
then:
  capability: package.create_request
`))
	if err != nil {
		t.Fatal(err)
	}
	if rule.Evaluate.DocType != "Balance" {
		t.Fatalf("legacy entity target was not normalized to doctype: %q", rule.Evaluate.DocType)
	}
	matched, err := EvaluateDeclarativeRule(context.Background(), rule, "package.stock.changed", nil, declarativeRuleEvaluator{result: true})
	if err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
	matched, err = EvaluateDeclarativeRule(context.Background(), rule, "package.other", nil, declarativeRuleEvaluator{result: true})
	if err != nil || matched {
		t.Fatalf("wrong event matched=%v err=%v", matched, err)
	}
}

func TestDeclarativeRuleRejectsConflictingLegacyEntityTarget(t *testing.T) {
	_, err := ParseDeclarativeRule([]byte(`
name: conflicting
namespace: package
version: 1
kind: rule
when:
  event: package.changed
evaluate:
  doctype: Stock Movement
  entity: Stock Balance
  predicate: quantity > 0
then:
  capability: package.inspect
`))
	if err == nil {
		t.Fatal("rule parser accepted conflicting doctype and legacy entity targets")
	}
}
