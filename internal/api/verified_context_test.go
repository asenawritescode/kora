package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
)

// newVerifiedTestContext returns a gin context carrying a verified execution for
// a human actor with the given roles and grants, bound to one tenant and site.
func newVerifiedTestContext(t *testing.T, tenant, site string, roles []string, grants []policy.Grant) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	ctx.Set("site_name", site)
	ctx.Set("site_id", tenant)
	actor := contract.ActorContext{PrincipalID: "analytics-reader", PrincipalType: contract.PrincipalHuman, SubjectUserID: "analytics-reader", Site: site, Roles: roles, AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(tenant), actor, grants)
	if err != nil {
		t.Fatal("create verified test execution:", err)
	}
	ctx.Set("policy_execution", execution)
	return ctx
}
