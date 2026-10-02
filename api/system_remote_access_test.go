package api

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

func TestCanInspectDocTypeScopesDelegatedCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	c.Set("auth_type", "extension")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "Work Order", Read: true}})

	if !canInspectDocType(c, "Work Order") {
		t.Fatal("extension should inspect a permitted DocType")
	}
	if canInspectDocType(c, "Payroll") {
		t.Fatal("extension must not inspect an ungranted DocType")
	}
}

func TestCanInspectDocTypeKeepsSessionBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	if !canInspectDocType(c, "Work Order") {
		t.Fatal("normal authenticated session behavior changed")
	}
}
