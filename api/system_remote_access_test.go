package api

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func TestCanInspectDocTypeScopesDelegatedCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Work Order"})
	registry.Register(&doctype.DocType{Name: "Payroll"})
	c, _ := gin.CreateTestContext(nil)
	c.Set("auth_type", "extension")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "Work Order", Read: true}})
	c.Set("policy_execution", routerTestExecution(nil, []policy.Grant{{Resource: "Work Order", Operation: "read"}}))

	if !canInspectDocType(c, registry, "Work Order") {
		t.Fatal("extension should inspect a permitted DocType")
	}
	if canInspectDocType(c, registry, "Payroll") {
		t.Fatal("extension must not inspect an ungranted DocType")
	}
}

func TestCanInspectDocTypeKeepsSessionBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Work Order"})
	registry.Permissions.AddRole(&doctype.Role{Name: doctype.AdminRole})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Work Order", Role: doctype.AdminRole, Read: true})
	c, _ := gin.CreateTestContext(nil)
	c.Set("policy_execution", routerTestExecution([]string{doctype.AdminRole}, nil))
	if !canInspectDocType(c, registry, "Work Order") {
		t.Fatal("normal authenticated session behavior changed")
	}
}
