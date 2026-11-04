package api

import (
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func TestScriptIdentityUsesVerifiedExecution(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("tenant-stable", contract.ActorContext{
		PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, SubjectUserID: "alice@example.test",
		Site: "acme", Roles: []string{"Reader", "Approver"},
	})
	if err != nil {
		t.Fatal(err)
	}

	user, roles, ok := scriptIdentity(execution, "acme")
	if !ok || user != "alice@example.test" || len(roles) != 2 || roles[0] != "Reader" {
		t.Fatalf("scriptIdentity = (%q, %#v, %v), want verified actor", user, roles, ok)
	}
	roles[0] = "Administrator"
	if execution.Actor().Roles[0] != "Reader" {
		t.Fatal("script identity roles alias the immutable execution actor")
	}
	if _, _, ok := scriptIdentity(execution, "other-tenant"); ok {
		t.Fatal("execution from a different tenant route was accepted")
	}
	if _, _, ok := scriptIdentity(policy.Execution{}, "acme"); ok {
		t.Fatal("missing verified execution was accepted")
	}
}

func TestRequestUserIDAndAttributionIgnoreLegacyGinUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("user", "spoofed@example.test")
	c.Set("site_name", "acme")
	c.Set("site_id", "tenant-stable")
	execution, err := policy.NewVerifiedExecution("tenant-stable", contract.ActorContext{
		PrincipalID: "extension:trusted", PrincipalType: contract.PrincipalService, SubjectUserID: "alice@example.test", Site: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Set("policy_execution", execution)
	if got, ok := requestUserID(c); !ok || got != "alice@example.test" {
		t.Fatalf("requestUserID = (%q, %v), want verified subject", got, ok)
	}
	if got := currentUser(c); got != "alice@example.test" {
		t.Fatalf("currentUser = %q, want verified subject", got)
	}

	c.Set("site_id", "another-tenant")
	if _, ok := requestUserID(c); ok {
		t.Fatal("verified identity was accepted under a different stable tenant route")
	}
	c.Set("policy_execution", policy.Execution{})
	if got := currentUser(c); got == "spoofed@example.test" {
		t.Fatalf("unverified Gin user was used for attribution: %q", got)
	}
}
