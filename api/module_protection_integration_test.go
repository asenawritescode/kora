//go:build integration

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/schema"
	"github.com/gin-gonic/gin"
)

func TestHTTPAndPublicFormCannotUseGenericMutationOnCommandOnlyResources(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	protected := &doctype.DocType{
		Name: "ProtectedInvoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands),
		Fields:       []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}},
		PublicAccess: &doctype.PublicAccess{Enabled: true, Fields: []string{"title"}},
	}
	registry.Register(protected)
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: protected.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true})
	if err := schema.MigrateSiteFromRegistry(database, site, registry, kdb.Resolve("mysql")); err != nil {
		t.Fatal("migrate protected module resource:", err)
	}
	registry.Views.Register(&doctype.View{
		Name: "Public protected invoice", Route: "/protected-invoice", SourceDocType: protected.Name,
		PublicAccess: &doctype.ViewPublicAccess{Enabled: true, AllowMutations: true},
	})

	httpResult := serveResourceMutationBody(t, handler, database, registry, site, httpMethodPost,
		"/api/resource/"+protected.Name, `{"title":"administrator generic bypass"}`, "", []string{doctype.AdminRole})
	if httpResult.Code != http.StatusForbidden {
		t.Fatalf("HTTP generic create of command-only resource = %d, want forbidden: %s", httpResult.Code, httpResult.Body.String())
	}

	forgedRecorder := httptest.NewRecorder()
	forgedContext, _ := gin.CreateTestContext(forgedRecorder)
	forgedContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/kernel/record.create", strings.NewReader(`{"module_execution":{"tenant_id":"`+site+`","caller_module_id":"finance.ar","owner_module_id":"finance.ar","binding_revision":1,"allowed_operation":"record.create","authorized_command":"record.create"},"payload":{"doctype":"ProtectedInvoice","data":{"title":"forged module identity"}}}`))
	forgedContext.Request.Header.Set("Content-Type", "application/json")
	forgedContext.Params = gin.Params{{Key: "command", Value: kernel.CommandRecordCreate}}
	forgedContext.Set("site_name", site)
	forgedContext.Set("site_db", database)
	forgedContext.Set("site_registry", registry)
	forgedContext.Set("site_db_type", "mysql")
	forgedContext.Set("policy_execution", integrationTestExecution(t, site, "admin@example.test", []string{doctype.AdminRole}))
	handler.HandleKernelOperation(forgedContext)
	if forgedRecorder.Code != http.StatusForbidden {
		t.Fatalf("forged HTTP module_execution status = %d, want forbidden: %s", forgedRecorder.Code, forgedRecorder.Body.String())
	}

	gin.SetMode(gin.TestMode)
	publicRecorder := httptest.NewRecorder()
	publicContext, _ := gin.CreateTestContext(publicRecorder)
	publicContext.Request = httptest.NewRequest(http.MethodPost, "/api/v1/v?route=/protected-invoice", strings.NewReader(`{"title":"public generic bypass"}`))
	publicContext.Request.Header.Set("Content-Type", "application/json")
	publicContext.Set("site_name", site)
	publicContext.Set("site_db", database)
	publicContext.Set("site_registry", registry)
	publicContext.Set("site_db_type", "mysql")
	handler.HandlePublicCreate(publicContext)
	if publicRecorder.Code != http.StatusForbidden {
		t.Fatalf("public form create of command-only resource = %d, want forbidden: %s", publicRecorder.Code, publicRecorder.Body.String())
	}

	var rows, completedAudits, outboxEvents, receipts int
	dialect := kdb.Resolve("mysql")
	if err := database.QueryRow("SELECT COUNT(*) FROM " + dialect.QuoteIdent(protected.RawTableName())).Scan(&rows); err != nil {
		t.Fatal("count protected resource rows:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name IN ('record.create','public_form.submit') AND status='completed'", site).Scan(&completedAudits); err != nil {
		t.Fatal("count completed generic mutation audits:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site=?", site).Scan(&outboxEvents); err != nil {
		t.Fatal("count generic mutation outbox events:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name IN ('record.create','public_form.submit')", site).Scan(&receipts); err != nil {
		t.Fatal("count generic mutation receipts:", err)
	}
	if rows != 0 || completedAudits != 0 || outboxEvents != 0 || receipts != 0 {
		t.Fatalf("denied HTTP/public-form bypasses left rows=%d completed_audits=%d outbox=%d receipts=%d", rows, completedAudits, outboxEvents, receipts)
	}
}
