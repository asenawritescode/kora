//go:build integration

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

func TestOfflinePushReceiptReplayAndIndependentRejection(t *testing.T) {
	h, db, reg, site := newResourceIntegrationHandler(t)
	serve := func(body string) map[string]any {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/offline/push", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Set("site_name", site)
		ctx.Set("site_db", db)
		ctx.Set("site_registry", reg)
		ctx.Set("site_db_type", "mysql")
		ctx.Set("policy_execution", integrationTestExecution(t, site, "admin@example.test", []string{doctype.AdminRole}))
		h.HandleOfflinePush(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("push HTTP %d: %s", recorder.Code, recorder.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response["data"].(map[string]any)
	}
	batch := `{"operations":[{"id":"bad","idempotency_key":"offline-bad","command":"record.create","doctype":"TestDoc","payload":{"title":""}},{"id":"good","idempotency_key":"offline-good","command":"record.create","doctype":"TestDoc","payload":{"title":"Offline item"}}]}`
	first := serve(batch)["receipts"].([]any)
	if first[0].(map[string]any)["status"] != "rejected" || first[1].(map[string]any)["status"] != "acked" {
		t.Fatalf("per-operation statuses = %#v", first)
	}
	name := first[1].(map[string]any)["record"].(map[string]any)["name"].(string)
	second := serve(batch)["receipts"].([]any)
	if second[1].(map[string]any)["replayed"] != true || second[1].(map[string]any)["record"].(map[string]any)["name"] != name {
		t.Fatalf("lost-response replay did not return original receipt: %#v", second[1])
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabTestDoc` WHERE name = ?", name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replayed create wrote %d records", count)
	}
}
