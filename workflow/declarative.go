package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"gopkg.in/yaml.v3"
)

// DeclarativeRule is the provider-neutral shape for an event-triggered rule.
// The predicate is intentionally data: evaluation is delegated to the shared
// rule evaluator supplied by the caller.
type DeclarativeRule struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Version   int    `yaml:"version"`
	Kind      string `yaml:"kind"`
	When      struct {
		Event string `yaml:"event"`
	} `yaml:"when"`
	Evaluate struct {
		// DocType is the target name for the record context. It resolves through
		// the canonical DocType registry; there is no parallel Entity schema.
		DocType   string `yaml:"doctype"`
		Predicate string `yaml:"predicate"`
	} `yaml:"evaluate"`
	Then struct {
		CreateTask       string `yaml:"create_task"`
		Capability       string `yaml:"capability"`
		RequiresApproval bool   `yaml:"requires_approval"`
	} `yaml:"then"`
}

// DeclarativeStep is one capability invocation in a declarative workflow.
type DeclarativeStep struct {
	ID string `yaml:"id"`
	// DocType and Operation are the canonical permission-first form.
	DocType   string `yaml:"doctype,omitempty"`
	Operation string `yaml:"operation,omitempty"`
	// Capability is retained for legacy package definitions during migration.
	Capability string `yaml:"capability,omitempty"`
	Actor      string `yaml:"actor"`
}

// DeclarativeWorkflow is a strict, versioned procedure whose effects enter
// through the supplied StepExecutor.
type DeclarativeWorkflow struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Version   int               `yaml:"version"`
	Kind      string            `yaml:"kind"`
	Trigger   string            `yaml:"trigger"`
	Steps     []DeclarativeStep `yaml:"steps"`
}

func ParseDeclarativeRule(raw []byte) (DeclarativeRule, error) {
	var rule DeclarativeRule
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&rule); err != nil {
		return rule, fmt.Errorf("rule definition: %w", err)
	}
	if rule.Name == "" || rule.Namespace == "" || rule.Version <= 0 || rule.Kind != "rule" || rule.When.Event == "" || rule.Evaluate.DocType == "" || rule.Evaluate.Predicate == "" || rule.Then.Capability == "" {
		return rule, fmt.Errorf("rule requires name, namespace, positive version, kind rule, trigger event, doctype, predicate, and capability")
	}
	return rule, nil
}

func ParseDeclarativeWorkflow(raw []byte) (DeclarativeWorkflow, error) {
	var workflow DeclarativeWorkflow
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&workflow); err != nil {
		return workflow, fmt.Errorf("workflow definition: %w", err)
	}
	if workflow.Name == "" || workflow.Namespace == "" || workflow.Version <= 0 || workflow.Kind != "workflow" || workflow.Trigger == "" || len(workflow.Steps) == 0 {
		return workflow, fmt.Errorf("workflow requires name, namespace, positive version, kind workflow, trigger, and steps")
	}
	seen := map[string]bool{}
	for _, step := range workflow.Steps {
		if step.ID == "" || (step.Capability == "" && (step.DocType == "" || step.Operation == "")) || seen[step.ID] {
			return workflow, fmt.Errorf("workflow has invalid or duplicate step")
		}
		seen[step.ID] = true
	}
	return workflow, nil
}

// RuleEvaluator is injected so the declarative seam uses the engine's shared,
// deterministic expression evaluator rather than embedding domain predicates.
type RuleEvaluator interface {
	Evaluate(context.Context, string, any) (bool, error)
}

// EvaluateDeclarativeRule applies the generic event gate and delegates the
// predicate to the shared expression/rule evaluator. A false predicate is a
// normal no-op and never creates a workflow instance.
func EvaluateDeclarativeRule(ctx context.Context, rule DeclarativeRule, event string, input any, evaluator RuleEvaluator) (bool, error) {
	if evaluator == nil {
		return false, fmt.Errorf("rule evaluator is required")
	}
	if rule.When.Event != event {
		return false, nil
	}
	return evaluator.Evaluate(ctx, rule.Evaluate.Predicate, input)
}

// StepExecutor is the same capability executor used by human, agent, and API
// callers. The workflow adapter never receives a database handle.
type StepExecutor interface {
	Execute(context.Context, string, contract.ActorContext, any) error
}

// PermissionStepExecutor is implemented by the production executor. It lets
// workflows invoke the same DocType permission path as API and agent actors.
type PermissionStepExecutor interface {
	ExecutePermission(context.Context, string, string, contract.ActorContext, any) error
}

// RunDeclarativeWorkflow executes steps in declaration order, enforcing the
// actor class declared by each step. Human-gated steps reject non-human actors.
func RunDeclarativeWorkflow(ctx context.Context, workflow DeclarativeWorkflow, actor contract.ActorContext, input any, executor StepExecutor) ([]string, error) {
	if executor == nil {
		return nil, fmt.Errorf("workflow executor is required")
	}
	if !actor.Authenticated() {
		return nil, contract.NewError(contract.CodeUnauthenticated, "authenticated actor is required")
	}
	executed := make([]string, 0, len(workflow.Steps))
	for _, step := range workflow.Steps {
		if requiresHuman(step.Actor) && actor.PrincipalType != contract.PrincipalHuman {
			return executed, contract.NewError(contract.CodePermissionDenied, "workflow step requires human approval")
		}
		var err error
		if step.DocType != "" && step.Operation != "" {
			permissionExecutor, ok := executor.(PermissionStepExecutor)
			if !ok {
				return executed, fmt.Errorf("workflow step %q requires a permission-aware executor", step.ID)
			}
			err = permissionExecutor.ExecutePermission(ctx, step.DocType, step.Operation, actor, input)
		} else {
			err = executor.Execute(ctx, step.Capability, actor, input)
		}
		if err != nil {
			return executed, fmt.Errorf("workflow step %q: %w", step.ID, err)
		}
		executed = append(executed, step.ID)
	}
	return executed, nil
}

func requiresHuman(actor string) bool { return strings.EqualFold(strings.TrimSpace(actor), "human") }
