package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/recon"
	"github.com/gin-gonic/gin"
)

type businessFindingSummaryAuthorization struct {
	execution policy.Execution
	roles     policy.RolePolicy
}

func (a businessFindingSummaryAuthorization) CanInspectBusinessFindings(_ context.Context, scope contract.BusinessScope, actor string) error {
	if !a.execution.Valid() || a.execution.Tenant() != scope.TenantID {
		return errors.New("verified tenant execution is required")
	}
	identity := a.execution.Actor()
	subject := strings.TrimSpace(identity.SubjectUserID)
	if subject == "" {
		subject = strings.TrimSpace(identity.PrincipalID)
	}
	if actor == "" || actor != subject {
		return errors.New("business finding summary actor does not match verified execution")
	}
	_, authErr := policy.DecideForBusinessScope(a.execution, a.roles, "module.business_reconciliation.findings", "read", scope, scope.Kind == contract.ScopeKindLegalEntity)
	if authErr != nil {
		return authErr
	}
	return nil
}

// HandleBusinessReconciliationSummary returns only unresolved business
// findings for the caller's exact scope. Delivery failures remain a separate
// inspection surface.
func (h *Handler) HandleBusinessReconciliationSummary(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	actor, ok := executionUserID(execution)
	if !ok {
		writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation inspection is not granted", nil)
		return
	}
	service := h.runtimeService(c, c.GetString("site_name")).BusinessReconciliation
	if service == nil {
		writeError(c, http.StatusServiceUnavailable, "reconciliation.unavailable", "Business reconciliation inspection is unavailable", nil)
		return
	}
	count, err := service.CountUnresolvedFindings(c.Request.Context(), scope, actor, businessFindingSummaryAuthorization{execution: execution, roles: h.siteRegistry(c)})
	if err != nil {
		if errors.Is(err, recon.ErrBusinessFindingSummaryDenied) {
			writeError(c, http.StatusForbidden, "permission.denied", "Business reconciliation inspection is not granted", nil)
			return
		}
		writeError(c, http.StatusServiceUnavailable, "reconciliation.unavailable", "Business reconciliation inspection is unavailable", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"scope": scope, "unresolved_business_findings": count}})
}
