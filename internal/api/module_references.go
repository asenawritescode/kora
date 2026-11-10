package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
)

// newBusinessReferenceValidator binds field validation to the authenticated
// request's immutable module plan and exact business scope.
func newBusinessReferenceValidator(ctx context.Context, services SiteRuntimeService, execution policy.Execution, scope contract.BusinessScope) contract.BusinessReferenceValidator {
	if ctx == nil || !execution.Valid() || scope.Validate() != nil || execution.Tenant() != scope.TenantID {
		return nil
	}
	var once sync.Once
	var planErr error
	var plan plugin.BindingPlan
	return func(consumer contract.ModuleID, rule contract.ReferenceRule, raw json.RawMessage) error {
		if services.ResolveModuleBindings == nil {
			return fmt.Errorf("business reference provider plan is unavailable")
		}
		var ref contract.BusinessRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return fmt.Errorf("business reference must be a provider-qualified object")
		}
		once.Do(func() { plan, planErr = services.ResolveModuleBindings(scope) })
		if planErr != nil {
			return fmt.Errorf("business reference provider plan is unavailable: %w", planErr)
		}
		_, err := plan.ResolveBusinessReference(ctx, execution, plugin.BusinessReferenceRequest{Scope: scope, Consumer: consumer, Rule: rule, Ref: ref})
		return err
	}
}
