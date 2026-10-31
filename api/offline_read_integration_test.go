//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

func offlineReadRequest(t *testing.T, h *Handler, db *sql.DB, reg *doctype.Registry, site, path, body, user string) map[string]any {
	t.Helper()
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	ctx.Set("policy_execution", integrationTestExecution(t, site, user, []string{doctype.AdminRole}))
	switch path {
	case "/api/v1/offline/bootstrap":
		h.HandleOfflineBootstrap(ctx)
	case "/api/v1/offline/pull":
		h.HandleOfflinePull(ctx)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s HTTP %d: %s", path, response.Code, response.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope["data"].(map[string]any)
}

func TestOfflineBootstrapSnapshotAndPullDeletion(t *testing.T) {
	h, db, reg, site := newResourceIntegrationHandler(t)
	create := func(title, key string) string {
		response := serveResourceMutation(t, h, db, reg, site, httpMethodPost, "/api/resource/TestDoc", title, key)
		if response.Code != http.StatusCreated {
			t.Fatalf("create HTTP %d: %s", response.Code, response.Body.String())
		}
		return requireRESTDocument(t, response.Body.Bytes(), "TestDoc")["name"].(string)
	}
	firstName := create("First", "bootstrap-first")
	secondName := create("Second", "bootstrap-second")
	first := offlineReadRequest(t, h, db, reg, site, "/api/v1/offline/bootstrap", `{"doctypes":["TestDoc"],"limit":1}`, "admin@example.test")
	if first["complete"] != false || len(first["records"].([]any)) != 1 {
		t.Fatalf("first snapshot page: %#v", first)
	}
	thirdName := create("Third", "bootstrap-third")
	page := offlineReadRequest(t, h, db, reg, site, "/api/v1/offline/bootstrap", `{"snapshot_token":"`+first["snapshot_token"].(string)+`","page_token":"`+first["next_page_token"].(string)+`","limit":1}`, "admin@example.test")
	if page["complete"] != true || len(page["records"].([]any)) != 1 {
		t.Fatalf("final snapshot page: %#v", page)
	}
	seen := map[string]bool{}
	for _, entry := range append(first["records"].([]any), page["records"].([]any)...) {
		seen[entry.(map[string]any)["record"].(map[string]any)["name"].(string)] = true
	}
	if !seen[firstName] || !seen[secondName] || seen[thirdName] {
		t.Fatalf("snapshot membership changed after page one: %#v", seen)
	}
	cursor := page["pull_cursor"].(string)
	pulled := offlineReadRequest(t, h, db, reg, site, "/api/v1/offline/pull", `{"cursor":"`+cursor+`"}`, "admin@example.test")
	if len(pulled["changes"].([]any)) != 1 || pulled["changes"].([]any)[0].(map[string]any)["record"].(map[string]any)["name"] != thirdName {
		var cursorSequence int64
		if err := db.QueryRow("SELECT source_sequence FROM _kora_offline_cursor WHERE token = ?", cursor).Scan(&cursorSequence); err != nil {
			t.Fatalf("post-snapshot create missing from pull: %#v (read cursor sequence: %v)", pulled, err)
		}
		rows, err := db.Query("SELECT source_sequence, event_type, aggregate_type, aggregate_id FROM _kora_outbox WHERE site = ? AND source_sequence > ? ORDER BY source_sequence", site, cursorSequence)
		if err != nil {
			t.Fatalf("post-snapshot create missing from pull: %#v (read outbox diagnostics: %v)", pulled, err)
		}
		var outboxEvents []string
		for rows.Next() {
			var sequence int64
			var eventType, aggregateType, aggregateID string
			if err := rows.Scan(&sequence, &eventType, &aggregateType, &aggregateID); err != nil {
				rows.Close()
				t.Fatalf("post-snapshot create missing from pull: %#v (scan outbox diagnostics: %v)", pulled, err)
			}
			outboxEvents = append(outboxEvents, fmt.Sprintf("%d %s %s/%s", sequence, eventType, aggregateType, aggregateID))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("post-snapshot create missing from pull: %#v (read outbox diagnostics: %v)", pulled, err)
		}
		rows.Close()
		t.Fatalf("post-snapshot create missing from pull: %#v; events after cursor %d: %v", pulled, cursorSequence, outboxEvents)
	}
	deleted := serveResourceMutation(t, h, db, reg, site, httpMethodDelete, "/api/resource/TestDoc/"+firstName, "", "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete HTTP %d: %s", deleted.Code, deleted.Body.String())
	}
	pulled = offlineReadRequest(t, h, db, reg, site, "/api/v1/offline/pull", `{"cursor":"`+pulled["next_cursor"].(string)+`"}`, "admin@example.test")
	if len(pulled["tombstones"].([]any)) != 1 || pulled["tombstones"].([]any)[0].(map[string]any)["name"] != firstName {
		t.Fatalf("delete tombstone missing: %#v", pulled)
	}
	if _, err := db.Exec("UPDATE _kora_offline_cursor SET expires_at = ? WHERE token = ?", "2020-01-01 00:00:00", pulled["next_cursor"].(string)); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/offline/pull", strings.NewReader(`{"cursor":"`+pulled["next_cursor"].(string)+`"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	ctx.Set("policy_execution", integrationTestExecution(t, site, "admin@example.test", []string{doctype.AdminRole}))
	h.HandleOfflinePull(ctx)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "offline.rebootstrap_required") {
		t.Fatalf("expired cursor response = HTTP %d: %s", response.Code, response.Body.String())
	}
}
