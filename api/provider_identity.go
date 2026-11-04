package api

import (
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

// bindProviderCallbackExecution installs a tenant-bound service identity only
// after the callback adapter has authenticated its provider credential. The
// exact grants keep provider callbacks from inheriting tenant Administrator.
func bindProviderCallbackExecution(c *gin.Context, principal string, grants []policy.Grant) bool {
	if c == nil || principal == "" {
		return false
	}
	site := c.GetString("site_name")
	tenant := c.GetString("site_id")
	if site == "" || tenant == "" {
		return false
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(tenant), contract.ActorContext{
		PrincipalID: principal, PrincipalType: contract.PrincipalService,
		Site: site, AuthenticatedAt: time.Now().UTC(),
	}, grants)
	if err != nil {
		return false
	}
	c.Set("policy_execution", execution)
	c.Set("auth_type", "provider_callback")
	return true
}
