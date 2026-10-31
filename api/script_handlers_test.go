package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func TestScriptManagementRequiresVerifiedAdministrator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(doctype.NewRegistry(), nil)
	request := func(execution policy.Execution) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/system/script", nil)
		c.Set("site_name", "tenant-a")
		c.Set("user", "attacker")
		c.Set("user_role", doctype.AdminRole)
		if execution.Valid() {
			c.Set("policy_execution", execution)
		}
		handler.HandleScriptList(c)
		return response
	}

	if got := request(policy.Execution{}).Code; got != http.StatusForbidden {
		t.Fatalf("unverified caller-supplied Administrator status=%d, want 403", got)
	}
	reader, err := policy.NewVerifiedExecution("tenant-stable", contract.ActorContext{
		PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, SubjectUserID: "reader", Site: "tenant-a", Roles: []string{"Reader"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := request(reader).Code; got != http.StatusForbidden {
		t.Fatalf("verified non-admin status=%d, want 403", got)
	}
	admin, err := policy.NewVerifiedExecution("tenant-stable", contract.ActorContext{
		PrincipalID: "admin", PrincipalType: contract.PrincipalHuman, SubjectUserID: "admin", Site: "tenant-a", Roles: []string{doctype.AdminRole},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := request(admin).Code; got != http.StatusOK {
		t.Fatalf("verified admin status=%d, want 200", got)
	}
}
