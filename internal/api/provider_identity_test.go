package api

import (
	"testing"

	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
)

func TestProviderCallbackExecutionUsesStableTenantAndExactGrants(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set("site_name", "tenant.example")
	c.Set("site_id", "tenant-stable-1")
	grants := []policy.Grant{{Resource: "Payment", Operation: "write"}}
	if !bindProviderCallbackExecution(c, "provider:mpesa", grants) {
		t.Fatal("bindProviderCallbackExecution rejected a complete tenant context")
	}
	execution, ok := requestExecution(c)
	if !ok || execution.Tenant() != "tenant-stable-1" {
		t.Fatalf("provider execution = (%v, %v), want stable tenant identity", execution, ok)
	}
	actor := execution.Actor()
	if actor.PrincipalID != "provider:mpesa" || actor.Site != "tenant.example" || actor.PrincipalType != "service" || len(actor.Roles) != 0 {
		t.Fatalf("provider actor = %+v, want roleless service identity", actor)
	}
	if _, allowed := execution.GrantFor("Payment", "write"); !allowed {
		t.Fatal("exact Payment write grant was not retained")
	}
	if _, allowed := execution.GrantFor("External Operation", "write"); allowed {
		t.Fatal("provider callback inherited an unrelated resource grant")
	}
	if c.GetString("auth_type") != "provider_callback" {
		t.Fatalf("auth_type = %q, want provider_callback", c.GetString("auth_type"))
	}
}

func TestProviderCallbackExecutionRequiresStableTenantID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set("site_name", "tenant.example")
	if bindProviderCallbackExecution(c, "provider:mpesa", nil) {
		t.Fatal("provider callback execution was created without stable tenant identity")
	}
}
