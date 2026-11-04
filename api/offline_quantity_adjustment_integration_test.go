//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/schema"
	"github.com/gin-gonic/gin"
)

func TestConfiguredSignedQuantityEffectIsAtomicAndIdempotent(t *testing.T) {
	h, database, registry, site := newResourceIntegrationHandler(t)
	registry.Register(&doctype.DocType{Name: "Inventory Bin", Fields: []doctype.Field{{Fieldname: "label", Fieldtype: "Data", Reqd: true}, {Fieldname: "units", Fieldtype: "Int", Reqd: true, ReadOnly: true}}})
	registry.Register(&doctype.DocType{Name: "Inventory Adjustment", AppendOnly: true, Fields: []doctype.Field{{Fieldname: "bin", Fieldtype: "Link", Options: "Inventory Bin", Reqd: true}, {Fieldname: "delta", Fieldtype: "Int", Reqd: true}, {Fieldname: "reason", Fieldtype: "Text", Reqd: true}}})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Inventory Bin", Role: doctype.AdminRole, Read: true, Write: true, Create: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Inventory Adjustment", Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true})
	if err := schema.MigrateSiteFromRegistry(database, site, registry, kdb.Resolve("mysql")); err != nil {
		t.Fatal(err)
	}
	created := serveResourceMutationBody(t, h, database, registry, site, httpMethodPost, "/api/resource/Inventory%20Bin", `{"label":"Shelf","units":5}`, "bin-adjust-1", []string{doctype.AdminRole})
	if created.Code != http.StatusCreated {
		t.Fatalf("create bin: %d %s", created.Code, created.Body.String())
	}
	bin := requireRESTDocument(t, created.Body.Bytes(), "Inventory Bin")["name"].(string)
	registry.Views.Register(&doctype.View{Name: "Configured adjustment", Route: "/configured-adjustment", Type: "form", SourceDocType: "Inventory Adjustment", Components: []doctype.ViewComponent{{ID: "entry", Type: "record_form", Actions: []doctype.ViewAction{{ID: "apply", Trigger: "on_submit", Type: "create_quantity_effect", Config: map[string]any{"target_doctype": "Inventory Adjustment", "related_doctype": "Inventory Bin", "source_record_field": "bin", "delta_field": "delta", "reason_field": "reason", "related_quantity_field": "units"}}}}}})
	invoke := func(key string, delta int, reason string) *httptest.ResponseRecorder {
		gin.SetMode(gin.TestMode)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		body, _ := json.Marshal(map[string]any{"view": "Configured adjustment", "component": "entry", "context": map[string]any{"bin": bin, "delta": delta, "reason": reason}})
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/view/action/apply", strings.NewReader(string(body)))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("Idempotency-Key", key)
		c.Params = gin.Params{{Key: "actionId", Value: "apply"}}
		c.Set("site_name", site)
		c.Set("site_db", database)
		c.Set("site_registry", registry)
		c.Set("site_db_type", "mysql")
		c.Set("user", "admin@example.test")
		c.Set("user_role", doctype.AdminRole)
		c.Set("user_roles", []string{doctype.AdminRole})
		execution, err := policy.NewVerifiedExecution(contract.TenantID(site), contract.ActorContext{PrincipalID: "admin@example.test", PrincipalType: contract.PrincipalHuman, SubjectUserID: "admin@example.test", Site: site, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		c.Set("policy_execution", execution)
		h.HandleViewAction(c)
		return rec
	}
	first := invoke("adjust-1", 3, "Delivery count")
	if first.Code != http.StatusOK {
		t.Fatalf("increase: %d %s", first.Code, first.Body.String())
	}
	name := requireActionDocument(t, first.Body.Bytes())["name"].(string)
	replay := invoke("adjust-1", 3, "Delivery count")
	if replay.Code != http.StatusOK || replay.Header().Get("X-Kora-Replay") != "true" || requireActionDocument(t, replay.Body.Bytes())["name"] != name {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	var units, adjustments int
	if err := database.QueryRow("SELECT units FROM `tabInventory Bin` WHERE name=?", bin).Scan(&units); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabInventory Adjustment`").Scan(&adjustments); err != nil {
		t.Fatal(err)
	}
	if units != 8 || adjustments != 1 {
		t.Fatalf("replay effects units=%d adjustments=%d", units, adjustments)
	}
	rejected := invoke("adjust-2", -9, "Shrinkage")
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("oversell: %d %s", rejected.Code, rejected.Body.String())
	}
	if err := database.QueryRow("SELECT units FROM `tabInventory Bin` WHERE name=?", bin).Scan(&units); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabInventory Adjustment`").Scan(&adjustments); err != nil {
		t.Fatal(err)
	}
	if units != 8 || adjustments != 1 {
		t.Fatalf("rejected effects units=%d adjustments=%d", units, adjustments)
	}
	missing := invoke("adjust-3", 1, "")
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing reason: %d %s", missing.Code, missing.Body.String())
	}
	changeCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	changeCtx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/kernel/record.update", nil)
	changeCtx.Set("site_name", site)
	changeCtx.Set("site_db", database)
	changeCtx.Set("site_registry", registry)
	changeCtx.Set("site_db_type", "mysql")
	changeCtx.Set("user", "admin@example.test")
	changeCtx.Set("user_role", doctype.AdminRole)
	changeCtx.Set("user_roles", []string{doctype.AdminRole})
	verified, err := policy.NewVerifiedExecution(contract.TenantID(site), contract.ActorContext{PrincipalID: "admin@example.test", PrincipalType: contract.PrincipalHuman, SubjectUserID: "admin@example.test", Site: site, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	changeCtx.Set("policy_execution", verified)
	update, _ := json.Marshal(kernel.RecordUpdatePayload{Doctype: "Inventory Adjustment", Name: name, Data: json.RawMessage(`{"reason":"changed"}`)})
	_, rejectedUpdate := h.executeKernelOperation(changeCtx, kernel.CommandRecordUpdate, kernelRequest{Payload: update, IdempotencyKey: "adjust-tamper-1"})
	if rejectedUpdate == nil || rejectedUpdate.Type != contract.CodeValidationFailed {
		t.Fatalf("append-only update was accepted or failed for another reason: %+v", rejectedUpdate)
	}
}
