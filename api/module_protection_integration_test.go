//go:build integration

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/gin-gonic/gin"
)

func TestHTTPAndPublicFormAdaptersCannotMutateCommandsOnlyResources(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	dt := registry.Get("TestDoc")
	dt.ModuleOwnerID = "finance.ar"
	dt.ModuleOwnershipMode = string(contract.MutationCommands)
	dt.PublicAccess = &doctype.PublicAccess{Enabled: true, Fields: []string{"title"}}
	registry.Register(dt)
	registry.Views.Register(&doctype.View{
		Name: "Protected public request", Route: "/protected-request", SourceDocType: dt.Name,
		PublicAccess: &doctype.ViewPublicAccess{Enabled: true, AllowMutations: true},
	})

	response := serveResourceMutationBody(t, handler, database, registry, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"must not be created"}`, "", []string{doctype.AdminRole})
	if response.Code != http.StatusForbidden {
		t.Fatalf("generic HTTP create status = %d, want forbidden: %s", response.Code, response.Body.String())
	}

	forgedResponse := httptest.NewRecorder()
	forgedContext, _ := gin.CreateTestContext(forgedResponse)
	forgedContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/kernel/record.create", strings.NewReader(`{"module_execution":{"tenant_id":"`+site+`","caller_module_id":"finance.ar","owner_module_id":"finance.ar","binding_revision":1,"allowed_operation":"record.create","authorized_command":"record.create"},"payload":{"doctype":"TestDoc","data":{"title":"forged context"}}}`))
	forgedContext.Request.Header.Set("Content-Type", "application/json")
	forgedContext.Params = gin.Params{{Key: "command", Value: kernel.CommandRecordCreate}}
	forgedContext.Set("site_name", site)
	forgedContext.Set("site_db", database)
	forgedContext.Set("site_registry", registry)
	forgedContext.Set("site_db_type", "mysql")
	forgedContext.Set("policy_execution", integrationTestExecution(t, site, "admin@example.test", []string{doctype.AdminRole}))
	handler.HandleKernelOperation(forgedContext)
	if forgedResponse.Code != http.StatusForbidden {
		t.Fatalf("forged public module_execution status = %d, want forbidden: %s", forgedResponse.Code, forgedResponse.Body.String())
	}

	gin.SetMode(gin.TestMode)
	publicResponse := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(publicResponse)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/v?route=/protected-request", strings.NewReader(`{"title":"public bypass"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_db_type", "mysql")
	handler.HandlePublicCreate(ctx)
	if publicResponse.Code != http.StatusForbidden {
		t.Fatalf("public form create status = %d, want forbidden: %s", publicResponse.Code, publicResponse.Body.String())
	}

	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM ` + "`tabTestDoc`").Scan(&rows); err != nil {
		t.Fatal("count protected records:", err)
	}
	if rows != 0 {
		t.Fatalf("generic HTTP/public form adapters persisted %d protected records", rows)
	}
}
