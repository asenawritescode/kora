//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"

	aiadapter "github.com/asenawritescode/kora/api/ai"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/workflow"
	workflowsql "github.com/asenawritescode/kora/workflow/sqlstore"
)

// TestResourceCRUDUsesKernelAndPreservesRESTContract exercises the actual
// compatibility adapter against MySQL. It deliberately verifies the response
// consumed by the MVP as well as the persisted mutation and kernel side effects.
func TestResourceCRUDUsesKernelAndPreservesRESTContract(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	created := serveResourceMutation(t, handler, db, reg, site, httpMethodPost, "/api/resource/TestDoc", "New from MVP", "create-key-1")
	if created.Code != 201 {
		t.Fatalf("create status = %d, want 201: %s", created.Code, created.Body.String())
	}
	createdDoc := requireRESTDocument(t, created.Body.Bytes(), "TestDoc")
	name, _ := createdDoc["name"].(string)
	if name == "" || createdDoc["title"] != "New from MVP" {
		t.Fatalf("create response lost REST document fields: %#v", createdDoc)
	}
	version, ok := createdDoc["_version"].(string)
	if !ok || version == "" {
		t.Fatalf("create response did not include an optimistic concurrency token: %#v", createdDoc)
	}
	replayed := serveResourceMutation(t, handler, db, reg, site, httpMethodPost, "/api/resource/TestDoc", "New from MVP", "create-key-1")
	if replayed.Code != 201 || replayed.Header().Get("X-Kora-Replay") != "true" {
		t.Fatalf("idempotent REST retry = HTTP %d replay=%q: %s", replayed.Code, replayed.Header().Get("X-Kora-Replay"), replayed.Body.String())
	}
	if replayDoc := requireRESTDocument(t, replayed.Body.Bytes(), "TestDoc"); replayDoc["name"] != name {
		t.Fatalf("idempotent retry returned a different document: got %#v, want %q", replayDoc, name)
	}

	changedPayload := serveResourceMutation(t, handler, db, reg, site, httpMethodPost, "/api/resource/TestDoc", "Different payload", "create-key-1")
	if changedPayload.Code != http.StatusConflict {
		t.Fatalf("reused idempotency key status = %d, want %d: %s", changedPayload.Code, http.StatusConflict, changedPayload.Body.String())
	}
	var conflictBody map[string]any
	if err := json.Unmarshal(changedPayload.Body.Bytes(), &conflictBody); err != nil {
		t.Fatalf("decode idempotency conflict response: %v", err)
	}
	conflict, ok := conflictBody["error"].(map[string]any)
	if !ok || conflict["code"] != "kernel.IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("idempotency conflict code = %#v, want kernel.IDEMPOTENCY_KEY_REUSED", conflictBody["error"])
	}

	updateBody, err := json.Marshal(map[string]string{"title": "Updated by MVP", "_version": version})
	if err != nil {
		t.Fatal("encode versioned update:", err)
	}
	updated := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut, "/api/resource/TestDoc/"+name, string(updateBody), "", []string{doctype.AdminRole})
	if updated.Code != 200 {
		t.Fatalf("update status = %d, want 200: %s", updated.Code, updated.Body.String())
	}
	updatedDoc := requireRESTDocument(t, updated.Body.Bytes(), "TestDoc")
	if updatedDoc["name"] != name || updatedDoc["title"] != "Updated by MVP" {
		t.Fatalf("update response contract mismatch: %#v", updatedDoc)
	}
	updatedVersion, ok := updatedDoc["_version"].(string)
	if !ok || updatedVersion == "" || updatedVersion == version {
		t.Fatalf("update response did not advance the version token: previous=%q response=%#v", version, updatedDoc)
	}
	latestBody, err := json.Marshal(map[string]string{"title": "Updated again", "_version": updatedVersion})
	if err != nil {
		t.Fatal("encode latest-version update:", err)
	}
	latestUpdate := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut, "/api/resource/TestDoc/"+name, string(latestBody), "", []string{doctype.AdminRole})
	if latestUpdate.Code != http.StatusOK {
		t.Fatalf("update with latest response token status = %d, want 200: %s", latestUpdate.Code, latestUpdate.Body.String())
	}
	staleBody, err := json.Marshal(map[string]string{"title": "Stale update must not win", "_version": version})
	if err != nil {
		t.Fatal("encode stale update:", err)
	}
	staleUpdate := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut, "/api/resource/TestDoc/"+name, string(staleBody), "", []string{doctype.AdminRole})
	if staleUpdate.Code != http.StatusConflict {
		t.Fatalf("stale update status = %d, want %d: %s", staleUpdate.Code, http.StatusConflict, staleUpdate.Body.String())
	}
	var persistedTitle string
	if err := db.QueryRow("SELECT title FROM `tabTestDoc` WHERE name = ?", name).Scan(&persistedTitle); err != nil || persistedTitle != "Updated again" {
		t.Fatalf("stale update overwrote the newer value: title=%q err=%v", persistedTitle, err)
	}

	deleted := serveResourceMutation(t, handler, db, reg, site, httpMethodDelete, "/api/resource/TestDoc/"+name, "", "")
	if deleted.Code != 200 {
		t.Fatalf("delete status = %d, want 200: %s", deleted.Code, deleted.Body.String())
	}
	var deleteResponse Response
	if err := json.Unmarshal(deleted.Body.Bytes(), &deleteResponse); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	deleteData, ok := deleteResponse.Data.(map[string]any)
	if !ok || deleteData["message"] != "deleted" {
		t.Fatalf("delete response contract mismatch: data=%#v", deleteResponse.Data)
	}

	var rows, audits, events int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabTestDoc` WHERE name = ?", name).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, name).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, name).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || audits != 4 || events != 4 {
		t.Fatalf("CRUD side effects = rows:%d audits:%d outbox:%d, want rows:0 audits:4 outbox:4 (one change event per mutation)", rows, audits, events)
	}
	var receipts int
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.create'", site).Scan(&receipts); err != nil {
		t.Fatalf("count create idempotency receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("create receipts = %d, want exactly one after replay and changed-payload conflict", receipts)
	}
}

func TestResourceCRUDKernelErrorMappingsRemainCompatible(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "TestDoc", Role: "Reader", Read: true})
	cases := []struct {
		name       string
		method     resourceHTTPMethod
		path       string
		body       string
		roles      []string
		wantStatus int
	}{
		{name: "invalid json", method: httpMethodPost, path: "/api/resource/TestDoc", body: "{", roles: []string{doctype.AdminRole}, wantStatus: http.StatusBadRequest},
		{name: "required field", method: httpMethodPost, path: "/api/resource/TestDoc", body: `{"title":""}`, roles: []string{doctype.AdminRole}, wantStatus: http.StatusBadRequest},
		{name: "unknown field", method: httpMethodPost, path: "/api/resource/TestDoc", body: `{"title":"valid","not_a_field":true}`, roles: []string{doctype.AdminRole}, wantStatus: http.StatusBadRequest},
		{name: "create denied", method: httpMethodPost, path: "/api/resource/TestDoc", body: `{"title":"denied"}`, roles: []string{"Reader"}, wantStatus: http.StatusForbidden},
		{name: "missing record", method: httpMethodPut, path: "/api/resource/TestDoc/MISSING-0001", body: `{"title":"updated"}`, roles: []string{doctype.AdminRole}, wantStatus: http.StatusNotFound},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := serveResourceMutationBody(t, handler, db, reg, site, test.method, test.path, test.body, "", test.roles)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error response: %v body=%s", err, response.Body.String())
			}
			if body["error"] == nil {
				t.Fatalf("compatibility error response missing error object: %#v", body)
			}
		})
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabTestDoc`").Scan(&rows); err != nil {
		t.Fatal("verify error cases left table readable:", err)
	}
	if rows != 0 {
		t.Fatalf("error cases wrote %d records", rows)
	}
}

func TestResourceAdapterHonorsExactGrantAndIdempotentReplay(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(site), contract.ActorContext{
		PrincipalID: "service:resource-writer", PrincipalType: contract.PrincipalService,
		Site: site, AuthenticatedAt: time.Now().UTC(),
	}, []policy.Grant{{Resource: "TestDoc", Operation: "create"}})
	if err != nil {
		t.Fatal("create verified service execution:", err)
	}
	create := func() *httptest.ResponseRecorder {
		return serveResourceMutationWithExecution(t, handler, database, registry, site, httpMethodPost,
			"/api/resource/TestDoc", `{"title":"delegated create"}`, "http-exact-grant-create-1", "mysql", execution)
	}
	first := create()
	if first.Code != http.StatusCreated {
		t.Fatalf("exact-grant create status = %d, want %d: %s", first.Code, http.StatusCreated, first.Body.String())
	}
	createdDoc := requireRESTDocument(t, first.Body.Bytes(), "TestDoc")
	name, _ := createdDoc["name"].(string)
	if name == "" {
		t.Fatalf("exact-grant create response omitted document name: %#v", createdDoc)
	}
	replay := create()
	if replay.Code != http.StatusCreated || replay.Header().Get("X-Kora-Replay") != "true" {
		t.Fatalf("exact-grant replay = HTTP %d, replay=%q: %s", replay.Code, replay.Header().Get("X-Kora-Replay"), replay.Body.String())
	}

	update := serveResourceMutationWithExecution(t, handler, database, registry, site, httpMethodPut,
		"/api/resource/TestDoc/"+url.PathEscape(name), `{"title":"unauthorized update"}`, "", "mysql", execution)
	if update.Code != http.StatusForbidden {
		t.Fatalf("create-only service grant authorized update: HTTP %d: %s", update.Code, update.Body.String())
	}
	var records, audits, outboxEvents, receipts int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabTestDoc` WHERE title = ?", "delegated create").Scan(&records); err != nil {
		t.Fatalf("count delegated records: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'", site).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", site).Scan(&outboxEvents); err != nil {
		t.Fatalf("count outbox events: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.create'", site).Scan(&receipts); err != nil {
		t.Fatalf("count idempotency receipts: %v", err)
	}
	if records != 1 || audits != 1 || outboxEvents != 1 || receipts != 1 {
		t.Fatalf("exact-grant create/replay effects = records:%d audits:%d outbox:%d receipts:%d, want one record, audit, receipt and outbox event", records, audits, outboxEvents, receipts)
	}
}

func TestAIAndHTTPAdaptersShareMutationReceipt(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(site), contract.ActorContext{
		PrincipalID: "service:shared-writer", PrincipalType: contract.PrincipalService,
		Site: site, AuthenticatedAt: time.Now().UTC(),
	}, []policy.Grant{{Resource: "TestDoc", Operation: "create"}})
	if err != nil {
		t.Fatal("create verified service execution:", err)
	}
	const clientKey = "shared-ai-http-retry-1"
	const title = "created once across adapters"
	aiKey := "channel-tool-" + func() string {
		digest := sha256.Sum256([]byte(site + "\x00" + clientKey))
		return hex.EncodeToString(digest[:])
	}()
	manager := &orm.TxManager{
		DB: database, Registry: registry, Dialect: kdb.Resolve("mysql"),
		SiteName: site, Outbox: outbox.NewSQLWriter(kdb.Resolve("mysql")), Execution: execution,
	}
	aiResult := aiadapter.ExecuteConfirmedToolWithExecution(manager, registry, "testdoc_create", map[string]any{"title": title}, execution.Actor().PrincipalID, site, clientKey, execution)
	if !strings.HasPrefix(aiResult, "Created TestDoc \"") {
		t.Fatalf("AI create failed: %s", aiResult)
	}

	retry := serveResourceMutationWithExecution(t, handler, database, registry, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"created once across adapters"}`, aiKey, "mysql", execution)
	if retry.Code != http.StatusCreated || retry.Header().Get("X-Kora-Replay") != "true" {
		t.Fatalf("HTTP retry did not replay AI receipt: HTTP %d replay=%q body=%s", retry.Code, retry.Header().Get("X-Kora-Replay"), retry.Body.String())
	}
	if document := requireRESTDocument(t, retry.Body.Bytes(), "TestDoc"); document["title"] != title {
		t.Fatalf("HTTP replay returned a different command result: %#v", document)
	}
	changed := serveResourceMutationWithExecution(t, handler, database, registry, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"changed command"}`, aiKey, "mysql", execution)
	if changed.Code != http.StatusConflict {
		t.Fatalf("cross-adapter key reuse with changed command = HTTP %d, want conflict: %s", changed.Code, changed.Body.String())
	}
	for query, want := range map[string]int{
		"SELECT COUNT(*) FROM `tabTestDoc` WHERE title = ?":                                                1,
		"SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'":               1,
		"SELECT COUNT(*) FROM _kora_outbox WHERE site = ?":                                                 1,
		"SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.create'": 1,
	} {
		var count int
		args := []any{site}
		if strings.Contains(query, "WHERE title = ?") {
			args = []any{title}
		}
		if err := database.QueryRow(query, args...).Scan(&count); err != nil {
			t.Fatalf("count shared adapter effects: %v", err)
		}
		if count != want {
			t.Errorf("shared adapter effect count for %q = %d, want %d", query, count, want)
		}
	}
}

func TestResourceCRUDUniqueConflictMapsTo409(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	first := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"first","serial":"shared"}`, "", []string{doctype.AdminRole})
	if first.Code != http.StatusCreated {
		t.Fatalf("create first record status = %d: %s", first.Code, first.Body.String())
	}
	second := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"second","serial":"other"}`, "", []string{doctype.AdminRole})
	if second.Code != http.StatusCreated {
		t.Fatalf("create second record status = %d: %s", second.Code, second.Body.String())
	}
	secondDoc := requireRESTDocument(t, second.Body.Bytes(), "TestDoc")
	name, _ := secondDoc["name"].(string)
	if name == "" {
		t.Fatal("second create response is missing its record name")
	}
	conflict := serveResourceMutationBody(t, handler, db, reg, site, httpMethodPut,
		"/api/resource/TestDoc/"+name, `{"serial":"shared"}`, "", []string{doctype.AdminRole})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("unique write conflict status = %d, want 409: %s", conflict.Code, conflict.Body.String())
	}
}

func TestResourceCRUDOutboxFailureRollsBackSequentialName(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	if _, err := database.Exec(`CREATE TRIGGER reject_outbox_insert
		BEFORE INSERT ON _kora_outbox FOR EACH ROW
		SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'forced outbox failure'`); err != nil {
		t.Fatalf("create outbox failure trigger: %v", err)
	}
	failed := serveResourceMutationBody(t, handler, database, registry, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"must roll back","serial":"rollback-name"}`, "", []string{doctype.AdminRole})
	if failed.Code == http.StatusCreated {
		t.Fatalf("create unexpectedly succeeded despite outbox failure: %s", failed.Body.String())
	}
	var businessRows, counterRows, audits, events int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabTestDoc` WHERE serial = ?", "rollback-name").Scan(&businessRows); err != nil {
		t.Fatalf("count rolled-back business rows: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_naming_series WHERE doctype = 'TestDoc' AND prefix = 'TEST'`).Scan(&counterRows); err != nil {
		t.Fatalf("count rolled-back sequence rows: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.create' AND status = 'completed'`, site).Scan(&audits); err != nil {
		t.Fatalf("count completed create audits: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox WHERE site = ?`, site).Scan(&events); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if businessRows != 0 || counterRows != 0 || audits != 0 || events != 0 {
		t.Fatalf("failed create left partial state: business=%d counters=%d audits=%d outbox=%d", businessRows, counterRows, audits, events)
	}
	if _, err := database.Exec(`DROP TRIGGER reject_outbox_insert`); err != nil {
		t.Fatalf("drop outbox failure trigger: %v", err)
	}
	succeeded := serveResourceMutationBody(t, handler, database, registry, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"first durable create","serial":"first-name"}`, "", []string{doctype.AdminRole})
	if succeeded.Code != http.StatusCreated {
		t.Fatalf("create after rollback status = %d: %s", succeeded.Code, succeeded.Body.String())
	}
	doc := requireRESTDocument(t, succeeded.Body.Bytes(), "TestDoc")
	if got := doc["name"]; got != "TEST-0001" {
		t.Fatalf("first committed name = %#v, want TEST-0001 after rollback", got)
	}
}

// TestScriptProviderMutationsUseKernelAndPreserveDocumentContract is a cutover
// gate for the script-facing CRUD adapter. Keep the provider's observable
// document contract while requiring the canonical operation ledger for all
// three mutations.
func TestScriptProviderMutationsUseKernelAndPreserveDocumentContract(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	tx := *handler.TxManager
	tx.CurrentUser = "script@example.test"
	tx.CurrentUserRole = doctype.AdminRole
	tx.CurrentUserRoles = []string{doctype.AdminRole}
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(site), contract.ActorContext{
		PrincipalID: "service:script-test", PrincipalType: contract.PrincipalService,
		Site: site, AuthenticatedAt: time.Now().UTC(),
	}, []policy.Grant{
		{Resource: "TestDoc", Operation: "read"},
		{Resource: "TestDoc", Operation: "create"},
		{Resource: "TestDoc", Operation: "write"},
		{Resource: "TestDoc", Operation: "delete"},
	})
	if err != nil {
		t.Fatal("create script service execution:", err)
	}
	tx.Execution = execution
	provider := NewScriptProvider(&tx, reg, site, nil, nil)

	created, err := provider.CreateDoc("TestDoc", map[string]any{
		"title": "created by script", "unlisted": "must not persist",
	}, "owner@example.test", "script@example.test")
	if err != nil {
		t.Fatalf("script create: %v", err)
	}
	name, _ := created["name"].(string)
	if name == "" || created["title"] != "created by script" {
		t.Fatalf("script create result lost document fields: %#v", created)
	}
	if created["unlisted"] != "must not persist" {
		t.Fatalf("script create result changed its input projection: %#v", created)
	}
	var owner, modifiedBy string
	if err := db.QueryRow("SELECT owner, modified_by FROM `tabTestDoc` WHERE name = ?", name).Scan(&owner, &modifiedBy); err != nil {
		t.Fatal("read script-created attribution:", err)
	}
	if owner != "service:script-test" || modifiedBy != "service:script-test" {
		t.Fatalf("script create attribution used non-verified identity: owner=%q modified_by=%q", owner, modifiedBy)
	}

	if err := provider.SaveDoc("TestDoc", map[string]any{
		"name": name, "title": "updated by script", "unlisted": "must not persist",
	}, "script@example.test"); err != nil {
		t.Fatalf("script update: %v", err)
	}
	updated, err := provider.GetDoc("TestDoc", name)
	if err != nil {
		t.Fatalf("read updated script document: %v", err)
	}
	if updated["title"] != "updated by script" {
		t.Fatalf("script update did not persist title: %#v", updated)
	}
	if _, ok := updated["unlisted"]; ok {
		t.Fatalf("script update exposed unknown persisted field: %#v", updated)
	}

	if err := provider.DeleteDoc("TestDoc", name); err != nil {
		t.Fatalf("script delete: %v", err)
	}
	var rows, audits, events int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabTestDoc` WHERE name = ?", name).Scan(&rows); err != nil {
		t.Fatal("count script-created document:", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, name).Scan(&audits); err != nil {
		t.Fatal("count script operation audit:", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, name).Scan(&events); err != nil {
		t.Fatal("count script outbox events:", err)
	}
	if rows != 0 || audits != 3 || events != 3 {
		t.Fatalf("script CRUD side effects = rows:%d audits:%d outbox:%d; want rows:0 audits:3 outbox:3, one change event per mutation", rows, audits, events)
	}
	var canonicalAudits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM _kora_operation_audit
		WHERE site = ? AND source = 'integration' AND actor_user = 'service:script-test'
		AND principal_id = 'service:script-test'
		AND command_name IN ('record.create', 'record.update', 'record.delete')`, site).Scan(&canonicalAudits); err != nil {
		t.Fatal("verify script actor and command audit:", err)
	}
	if canonicalAudits != 3 {
		t.Fatalf("canonical script audit rows=%d, want 3 with integration source and script actor", canonicalAudits)
	}
}

func TestScriptProviderDeniedMutationWritesNoRecord(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	reg.Permissions.SetPermission(&doctype.Permission{Doctype: "TestDoc", Role: "Reader", Read: true})
	tx := *handler.TxManager
	tx.CurrentUser = "reader@example.test"
	tx.CurrentUserRole = "Reader"
	tx.CurrentUserRoles = []string{"Reader"}
	tx.Execution = integrationTestExecution(t, site, tx.CurrentUser, tx.CurrentUserRoles)
	provider := NewScriptProvider(&tx, reg, site, nil, nil)
	if _, err := provider.CreateDoc("TestDoc", map[string]any{"title": "denied"}, "reader@example.test", "reader@example.test"); err == nil {
		t.Fatal("script provider created a record without the create grant")
	}
	var records, completed int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabTestDoc`").Scan(&records); err != nil {
		t.Fatal("count denied script writes:", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed' AND doctype = 'TestDoc'", site).Scan(&completed); err != nil {
		t.Fatal("count completed denied script audits:", err)
	}
	if records != 0 || completed != 0 {
		t.Fatalf("denied script mutation left records=%d completed_audits=%d", records, completed)
	}
}

func TestScriptHookSnapshotRefreshesForEachCommand(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	for _, ddl := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	for _, statement := range configstore.ConfigRevisionSchema(kdb.Resolve("mysql")) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(t.Context(), db, kdb.Resolve("mysql")); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}
	store := &script.Store{DB: db, Dialect: kdb.Resolve("mysql")}
	runner := &nestedScriptMutationRunner{}
	tx := *handler.TxManager
	tx.CurrentUser = "script@example.test"
	tx.CurrentUserRole = doctype.AdminRole
	tx.CurrentUserRoles = []string{doctype.AdminRole}
	tx.Execution = integrationTestExecution(t, site, tx.CurrentUser, tx.CurrentUserRoles)
	tx.ScriptRunner, tx.ScriptStore = runner, store
	provider := NewScriptProvider(&tx, reg, site, nil, nil)
	tx.ScriptProvider = provider
	if err := store.Insert(script.ScriptRecord{
		Name: "counter-hook", Site: site, ScriptType: script.TypeDocEvent,
		DocType: "TestDoc", Event: script.EventValidate, IsActive: true,
	}); err != nil {
		t.Fatalf("insert lifecycle script: %v", err)
	}
	for _, title := range []string{"first command", "second command"} {
		if _, err := provider.CreateDoc("TestDoc", map[string]any{"title": title}, "owner@example.test", "script@example.test"); err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
	}
	if runner.companionCalls != 2 {
		t.Fatalf("active hook executions=%d, want 2", runner.companionCalls)
	}
	disabled := false
	if err := store.Update(site, "counter-hook", script.ScriptUpdateRequest{IsActive: &disabled}, "admin@example.test"); err != nil {
		t.Fatalf("disable lifecycle script: %v", err)
	}
	if _, err := provider.CreateDoc("TestDoc", map[string]any{"title": "after disable"}, "owner@example.test", "script@example.test"); err != nil {
		t.Fatalf("create after script deactivation: %v", err)
	}
	if runner.companionCalls != 2 {
		t.Fatalf("inactive hook ran from a stale command cache: executions=%d", runner.companionCalls)
	}
}

func TestScriptHookProviderSkipsOnlyTheActiveLifecycleScript(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	for _, ddl := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	store := &script.Store{DB: db, Dialect: kdb.Resolve("mysql")}
	runner := &nestedScriptMutationRunner{}
	tx := *handler.TxManager
	tx.CurrentUser = "script@example.test"
	tx.CurrentUserRole = doctype.AdminRole
	tx.CurrentUserRoles = []string{doctype.AdminRole}
	tx.Execution = integrationTestExecution(t, site, tx.CurrentUser, tx.CurrentUserRoles)
	tx.ScriptRunner = runner
	tx.ScriptStore = store
	provider := NewScriptProvider(&tx, reg, site, nil, nil)
	tx.ScriptProvider = provider
	for _, name := range []string{"outer-hook", "companion-hook"} {
		if err := store.Insert(script.ScriptRecord{
			Name: name, Site: site, ScriptType: script.TypeDocEvent, DocType: "TestDoc",
			Event: script.EventValidate, IsActive: true, Script: "// exercised by the test runner",
		}); err != nil {
			t.Fatalf("insert lifecycle script %q: %v", name, err)
		}
	}

	created, err := provider.CreateDoc("TestDoc", map[string]any{"title": "outer"}, "owner@example.test", "script@example.test")
	if err != nil {
		t.Fatalf("outer script create: %v", err)
	}
	outerName := created["name"].(string)
	if runner.outerCalls != 1 || runner.companionCalls != 2 {
		t.Fatalf("hook calls outer=%d companion=%d, want 1 and 2", runner.outerCalls, runner.companionCalls)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabTestDoc` WHERE title IN ('outer', 'nested')").Scan(&count); err != nil {
		t.Fatal("count outer and nested script records:", err)
	}
	if count != 2 {
		t.Fatalf("script hook created %d records, want outer and nested", count)
	}
	var audits int
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed' AND doctype = 'TestDoc' AND doc_name <> ''", site).Scan(&audits); err != nil {
		t.Fatal("count outer and nested kernel audit:", err)
	}
	if audits != 2 || outerName == "" {
		t.Fatalf("kernel audit count=%d outer name=%q; want two audited writes", audits, outerName)
	}
}

func TestScriptHookNestedMutationRollsBackWithParent(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	for _, ddl := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	store := &script.Store{DB: db, Dialect: kdb.Resolve("mysql")}
	runner := &nestedScriptMutationRunner{rejectOuter: true}
	tx := *handler.TxManager
	tx.CurrentUser = "script@example.test"
	tx.CurrentUserRole = doctype.AdminRole
	tx.CurrentUserRoles = []string{doctype.AdminRole}
	tx.Execution = integrationTestExecution(t, site, tx.CurrentUser, tx.CurrentUserRoles)
	tx.ScriptRunner, tx.ScriptStore = runner, store
	provider := NewScriptProvider(&tx, reg, site, nil, nil)
	tx.ScriptProvider = provider
	for _, record := range []script.ScriptRecord{
		{Name: "outer-hook", Priority: 1, Site: site, ScriptType: script.TypeDocEvent, DocType: "TestDoc", Event: script.EventValidate, IsActive: true},
		{Name: "reject-hook", Priority: 2, Site: site, ScriptType: script.TypeDocEvent, DocType: "TestDoc", Event: script.EventValidate, IsActive: true},
	} {
		if err := store.Insert(record); err != nil {
			t.Fatalf("insert lifecycle script %q: %v", record.Name, err)
		}
	}
	if _, err := provider.CreateDoc("TestDoc", map[string]any{"title": "outer"}, "owner@example.test", "script@example.test"); err == nil {
		t.Fatal("outer mutation succeeded despite rejecting lifecycle hook")
	}
	var records, completedAudits, outboxRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabTestDoc` WHERE title IN ('outer', 'nested')").Scan(&records); err != nil {
		t.Fatal("count rolled-back script records:", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'", site).Scan(&completedAudits); err != nil {
		t.Fatal("count rolled-back operation audits:", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", site).Scan(&outboxRows); err != nil {
		t.Fatal("count rolled-back outbox rows:", err)
	}
	if records != 0 || completedAudits != 0 || outboxRows != 0 {
		t.Fatalf("rejected parent left partial state: records=%d completed_audits=%d outbox=%d", records, completedAudits, outboxRows)
	}
}

type nestedScriptMutationRunner struct {
	outerCalls     int
	companionCalls int
	rejectOuter    bool
}

func (r *nestedScriptMutationRunner) Execute(_ context.Context, req script.ExecuteRequest) (*script.ExecuteResult, error) {
	switch req.ScriptName {
	case "outer-hook":
		r.outerCalls++
		if req.Document["title"] == "outer" {
			if _, err := req.Provider.CreateDoc("TestDoc", map[string]any{"title": "nested"}, "owner@example.test", req.User); err != nil {
				return nil, err
			}
		}
	case "companion-hook":
		r.companionCalls++
	case "counter-hook":
		r.companionCalls++
	case "reject-hook":
		if r.rejectOuter && req.Document["title"] == "outer" {
			return nil, errors.New("intentional outer validation rejection")
		}
	}
	return &script.ExecuteResult{}, nil
}

func (*nestedScriptMutationRunner) Validate(string) error { return nil }
func (*nestedScriptMutationRunner) Close() error          { return nil }

func TestUnifiedWorkflowPolicyBlocksDirectStateWriteAndKeepsInternalTransitionAtomic(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "TestDoc", Role: "Reader", Read: true, Write: true, Create: true, Submit: true})
	dialect := kdb.Resolve("mysql")
	for _, statement := range configstore.ConfigRevisionSchema(dialect) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(t.Context(), database, dialect); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}
	definitionStore, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatalf("create workflow definition store: %v", err)
	}
	if err := definitionStore.Ensure(t.Context()); err != nil {
		t.Fatalf("create workflow definition tables: %v", err)
	}
	statusInput, err := json.Marshal(workflow.StatusActionConfig{StateField: "status", InitialState: "Draft", StatusTransition: workflow.StatusTransition{Action: "Complete", From: "Draft", To: "Done", AllowedRoles: []string{doctype.AdminRole}, RequireFields: []string{"serial"}, DocStatus: 1}})
	if err != nil {
		t.Fatal(err)
	}
	definition := contract.WorkflowDefinition{
		ID: "testdoc-status-action", Version: 1, Entry: "transition",
		Trigger: &contract.RecordTrigger{Kind: "record.updated", Table: "TestDoc", WatchedFields: []string{"status"}},
		Steps: []contract.StepDefinition{
			{ID: "transition", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "record.update_status", Version: 1, InputSchema: "kora.record.update_status", Input: statusInput, Next: "finish"}},
			{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
		},
	}
	tenant, err := contract.ParseTenantID(site)
	if err != nil {
		t.Fatalf("parse workflow tenant: %v", err)
	}
	actor, err := contract.ParsePrincipalID("system-workflow-integration")
	if err != nil {
		t.Fatalf("parse workflow migration actor: %v", err)
	}
	if err := definitionStore.PublishImportedBatch(t.Context(), tenant, actor, []contract.WorkflowDefinition{definition}, time.Now().UTC()); err != nil {
		t.Fatalf("publish v2 status-action definition: %v", err)
	}
	runtimeServices := NewSiteRuntimeServices()
	runtimeServices.Replace(site, SiteRuntimeService{WorkflowStatusPolicies: definitionStore, Outbox: outbox.NewSQLWriter(dialect)})
	handler.RuntimeServices = runtimeServices

	created := serveResourceMutationBody(t, handler, database, registry, site, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"Transition policy integration"}`, "v2-lifecycle-create", []string{doctype.AdminRole})
	if created.Code != http.StatusCreated {
		t.Fatalf("create TestDoc status=%d body=%s", created.Code, created.Body.String())
	}
	createdDoc := requireRESTDocument(t, created.Body.Bytes(), "TestDoc")
	name, ok := createdDoc["name"].(string)
	if !ok || name == "" {
		t.Fatalf("created record omitted its name: %#v", createdDoc)
	}
	var initialState string
	var initialDocStatus int
	if err := database.QueryRow("SELECT status, doc_status FROM `tabTestDoc` WHERE name = ?", name).Scan(&initialState, &initialDocStatus); err != nil {
		t.Fatalf("read initial lifecycle state: %v", err)
	}
	if initialState != "Draft" || initialDocStatus != 0 {
		t.Fatalf("new record lifecycle state=%q doc_status=%d, want Draft/0", initialState, initialDocStatus)
	}
	transition := func(roles []string, key string) *httptest.ResponseRecorder {
		t.Helper()
		execution := integrationTestExecution(t, site, "workflow@example.test", roles)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/test/workflow-action", strings.NewReader(`{"action":"Complete"}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Request.Header.Set("Idempotency-Key", key)
		ctx.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}, {Key: "name", Value: name}}
		ctx.Set("site_name", site)
		ctx.Set("site_id", site)
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_db_type", "mysql")
		ctx.Set("policy_execution", execution)
		_, transitionErr := handler.runKernelWorkflowTransition(ctx, "TestDoc", name, "Complete", 1)
		if transitionErr != nil {
			recorder.Body.WriteString(transitionErr.Message)
			recorder.Code = http.StatusBadRequest
		} else {
			recorder.Code = http.StatusOK
		}
		return recorder
	}
	if response := transition([]string{"Reader"}, "v2-lifecycle-reader-denied"); response.Code != http.StatusBadRequest {
		t.Fatalf("unauthorized lifecycle action status=%d, want 400: %s", response.Code, response.Body.String())
	}
	if response := serveResourceMutationBody(t, handler, database, registry, site, httpMethodPut,
		"/api/resource/TestDoc/"+url.PathEscape(name), `{"status":"Done"}`, "v2-lifecycle-direct-status", []string{doctype.AdminRole}); response.Code != http.StatusBadRequest {
		t.Fatalf("direct status write status=%d, want 400: %s", response.Code, response.Body.String())
	}
	if response := transition([]string{doctype.AdminRole}, "v2-lifecycle-required-field"); response.Code != http.StatusBadRequest {
		t.Fatalf("transition without required field status=%d, want 400: %s", response.Code, response.Body.String())
	}
	if response := serveResourceMutationBody(t, handler, database, registry, site, httpMethodPut,
		"/api/resource/TestDoc/"+url.PathEscape(name), `{"serial":"SERIAL-1"}`, "v2-lifecycle-set-required-field", []string{doctype.AdminRole}); response.Code != http.StatusOK {
		t.Fatalf("set required field status=%d, want 200: %s", response.Code, response.Body.String())
	}
	if response := transition([]string{doctype.AdminRole}, "v2-lifecycle-complete"); response.Code != http.StatusOK {
		t.Fatalf("authorized lifecycle action status=%d, want 200: %s", response.Code, response.Body.String())
	}
	var status string
	var docStatus int
	if err := database.QueryRow("SELECT status, doc_status FROM `tabTestDoc` WHERE name = ?", name).Scan(&status, &docStatus); err != nil {
		t.Fatalf("read transitioned status: %v", err)
	}
	if status != "Done" || docStatus != 1 {
		t.Fatalf("record status=%q doc_status=%d, want Done/1", status, docStatus)
	}
	var transitions int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND command_name = ? AND status = 'completed'", site, name, kernel.CommandWorkflowStatusTransition).Scan(&transitions); err != nil {
		t.Fatalf("count transition audit rows: %v", err)
	}
	if transitions != 1 {
		t.Fatalf("completed transition audit rows=%d, want 1", transitions)
	}
}

func TestCRMLeadAssignmentReadScopeHTTP(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)

	leadYAML, err := os.ReadFile("../config/v0/crm/doctypes/lead.yaml")
	if err != nil {
		t.Fatal("read CRM Lead config:", err)
	}
	leadType, err := doctype.ParseYAML(leadYAML, "lead.yaml")
	if err != nil {
		t.Fatal("parse CRM Lead config:", err)
	}
	registry.Register(leadType)
	permissionsYAML, err := os.ReadFile("../config/v0/crm/permissions.yaml")
	if err != nil {
		t.Fatal("read CRM permissions:", err)
	}
	permissions, err := doctype.ParsePermissionsYAML(permissionsYAML)
	if err != nil {
		t.Fatal("parse CRM permissions:", err)
	}
	for _, permission := range permissions {
		if permission.Doctype == "Lead" {
			registry.Permissions.SetPermission(permission)
		}
	}
	if err := schema.MigrateSiteFromRegistry(database, site, registry, kdb.Resolve("mysql")); err != nil {
		t.Fatal("migrate CRM Lead table:", err)
	}

	for _, record := range []struct {
		name, owner, leadName, assignee string
	}{
		{"LEAD-REP1", "manager@example.test", "Rep One Lead", "rep-one@example.test"},
		{"LEAD-REP2", "manager@example.test", "Rep Two Lead", "rep-two@example.test"},
		{"LEAD-REP1-OWN", "rep-one@example.test", "Rep One Owned Lead", ""},
		{"LEAD-REP2-OWN", "rep-two@example.test", "Rep Two Owned Lead", ""},
		{"LEAD-UNASSIGNED", "manager@example.test", "Unassigned Lead", ""},
	} {
		if _, err := database.Exec(`INSERT INTO `+"`tabLead`"+` (name, owner, lead_name, assignee_user) VALUES (?, ?, ?, ?)`, record.name, record.owner, record.leadName, record.assignee); err != nil {
			t.Fatalf("insert Lead %s fixture: %v", record.name, err)
		}
	}

	request := func(user, role, route string, list bool) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, route, nil)
		ctx.Params = gin.Params{{Key: "doctype", Value: "Lead"}}
		if !list {
			ctx.Params = append(ctx.Params, gin.Param{Key: "name", Value: strings.TrimPrefix(route, "/api/resource/Lead/")})
		}
		ctx.Set("site_name", site)
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_db_type", "mysql")
		ctx.Set("user", user)
		ctx.Set("user_role", role)
		ctx.Set("user_roles", []string{role})
		ctx.Set("policy_execution", integrationTestExecution(t, site, user, []string{role}))
		if list {
			handler.HandleList(ctx)
		} else {
			handler.HandleGet(ctx)
		}
		return recorder
	}

	decodeNames := func(response *httptest.ResponseRecorder) map[string]bool {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("Lead list status = %d, want 200: %s", response.Code, response.Body.String())
		}
		var payload struct {
			Data []struct {
				Name string `json:"name"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal("decode Lead list:", err)
		}
		names := make(map[string]bool, len(payload.Data))
		for _, row := range payload.Data {
			names[row.Name] = true
		}
		return names
	}

	for _, test := range []struct {
		user, own, ownByOwner, other, otherOwned string
	}{
		{"rep-one@example.test", "LEAD-REP1", "LEAD-REP1-OWN", "LEAD-REP2", "LEAD-REP2-OWN"},
		{"rep-two@example.test", "LEAD-REP2", "LEAD-REP2-OWN", "LEAD-REP1", "LEAD-REP1-OWN"},
	} {
		names := decodeNames(request(test.user, "Sales Rep", "/api/resource/Lead", true))
		if !names[test.own] || !names[test.ownByOwner] || names[test.other] || names[test.otherOwned] || names["LEAD-UNASSIGNED"] || len(names) != 2 {
			t.Errorf("Sales Rep %s list names = %#v, want only assigned %s and owned %s", test.user, names, test.own, test.ownByOwner)
		}
		for _, allowed := range []string{test.own, test.ownByOwner} {
			if response := request(test.user, "Sales Rep", "/api/resource/Lead/"+allowed, false); response.Code != http.StatusOK {
				t.Errorf("Sales Rep %s allowed Lead %s status = %d, want 200: %s", test.user, allowed, response.Code, response.Body.String())
			}
		}
		for _, denied := range []string{test.other, test.otherOwned, "LEAD-UNASSIGNED"} {
			if response := request(test.user, "Sales Rep", "/api/resource/Lead/"+denied, false); response.Code != http.StatusNotFound {
				t.Errorf("Sales Rep %s other Lead %s status = %d, want 404: %s", test.user, denied, response.Code, response.Body.String())
			}
		}
	}

	managerNames := decodeNames(request("manager@example.test", "Sales Manager", "/api/resource/Lead", true))
	if len(managerNames) != 5 || !managerNames["LEAD-REP1"] || !managerNames["LEAD-REP2"] || !managerNames["LEAD-REP1-OWN"] || !managerNames["LEAD-REP2-OWN"] || !managerNames["LEAD-UNASSIGNED"] {
		t.Errorf("Sales Manager list names = %#v, want all five Leads", managerNames)
	}
}

type captureAsyncWorkflowSink struct {
	requests []orm.AsyncHookRequest
	err      error
}

type workflowActionScriptMutationBus struct {
	onPublish func() error
}

type workflowConfigMutationRunner struct {
	update func() error
}

func (r workflowConfigMutationRunner) Execute(_ context.Context, req script.ExecuteRequest) (*script.ExecuteResult, error) {
	if req.ScriptName == "workflow-gate" && r.update != nil {
		if err := r.update(); err != nil {
			return nil, err
		}
	}
	return &script.ExecuteResult{}, nil
}
func (workflowConfigMutationRunner) Validate(string) error { return nil }
func (workflowConfigMutationRunner) Close() error          { return nil }

func (b workflowActionScriptMutationBus) Publish(eventstream.ChangeEvent) error {
	if b.onPublish != nil {
		return b.onPublish()
	}
	return nil
}
func (workflowActionScriptMutationBus) Subscribe() (<-chan eventstream.ChangeEvent, error) {
	return make(chan eventstream.ChangeEvent), nil
}
func (workflowActionScriptMutationBus) DrainWAL(func(eventstream.ChangeEvent)) (int, error) {
	return 0, nil
}
func (workflowActionScriptMutationBus) RotateWAL() (string, error)     { return "", nil }
func (workflowActionScriptMutationBus) CommitWALRotation(string) error { return nil }
func (workflowActionScriptMutationBus) Dropped() int64                 { return 0 }
func (workflowActionScriptMutationBus) Close() error                   { return nil }

func (s *captureAsyncWorkflowSink) Enqueue(_ context.Context, request orm.AsyncHookRequest) error {
	if s.err != nil {
		return s.err
	}
	s.requests = append(s.requests, request)
	return nil
}

type workflowTransitionFailureRunner struct{ calls []string }

func (r *workflowTransitionFailureRunner) Execute(_ context.Context, request script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.calls = append(r.calls, request.ScriptName)
	if request.ScriptName == "workflow-gate" {
		return nil, errors.New("gate rejected transition")
	}
	return &script.ExecuteResult{}, nil
}
func (*workflowTransitionFailureRunner) Validate(string) error { return nil }
func (*workflowTransitionFailureRunner) Close() error          { return nil }

func TestConfigRevisionFenceSerializesConcurrentTenantConfigWrite(t *testing.T) {
	_, database, _, _ := newResourceIntegrationHandler(t)
	for _, statement := range configstore.ConfigRevisionSchema(kdb.Resolve("mysql")) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(t.Context(), database, kdb.Resolve("mysql")); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}

	fenceTx, err := database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin transition transaction: %v", err)
	}
	defer fenceTx.Rollback()
	if err := configstore.FenceConfigRevisionTx(t.Context(), fenceTx, kdb.Resolve("mysql"), 1); err != nil {
		t.Fatalf("lock selected config revision: %v", err)
	}

	started := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeTx, beginErr := database.BeginTx(t.Context(), nil)
		if beginErr != nil {
			writeDone <- fmt.Errorf("begin competing config write: %w", beginErr)
			return
		}
		defer writeTx.Rollback()
		close(started)
		if bumpErr := configstore.BumpConfigRevisionTx(writeTx, kdb.Resolve("mysql")); bumpErr != nil {
			writeDone <- fmt.Errorf("bump competing config revision: %w", bumpErr)
			return
		}
		writeDone <- writeTx.Commit()
	}()
	<-started
	select {
	case err := <-writeDone:
		t.Fatalf("competing config write completed while transition held the revision fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := fenceTx.Commit(); err != nil {
		t.Fatalf("commit fenced transition: %v", err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("competing config write after transition commit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("config write remained blocked after transition released the fence")
	}
	revision, err := configstore.CurrentConfigRevision(t.Context(), database, kdb.Resolve("mysql"))
	if err != nil || revision != 2 {
		t.Fatalf("tenant config revision after serialized write = %d, err=%v; want 2", revision, err)
	}
}

func TestManifestRecordActionsUseKernelAndFilterTransportFields(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	reg.Views.Register(&doctype.View{
		Name: "Test actions", Route: "/test-actions", Type: "form", SourceDocType: "TestDoc",
		Components: []doctype.ViewComponent{{ID: "editor", Actions: []doctype.ViewAction{
			{ID: "create", Trigger: "on_submit", Type: "create_record", Config: map[string]any{"target_doctype": "TestDoc"}},
			{ID: "update", Trigger: "on_submit", Type: "update_record", Config: map[string]any{"target_doctype": "TestDoc"}},
		}}},
	})

	create := serveManifestAction(t, handler, site, db, reg, "create", map[string]any{
		"title": "Created through action", "name": "client-controlled-name", "_doctype": "OtherDoc", "unlisted": "discard",
	})
	if create.Code != http.StatusOK {
		t.Fatalf("manifest create status = %d: %s", create.Code, create.Body.String())
	}
	created := requireActionDocument(t, create.Body.Bytes())
	name, _ := created["name"].(string)
	if name == "" || name == "client-controlled-name" || created["title"] != "Created through action" {
		t.Fatalf("manifest create response = %#v", created)
	}
	if _, leaked := created["unlisted"]; leaked {
		t.Fatalf("manifest create included unrecognized field: %#v", created)
	}

	update := serveManifestAction(t, handler, site, db, reg, "update", map[string]any{
		"name": name, "title": "Updated through action", "_doctype": "OtherDoc", "unlisted": "discard",
	})
	if update.Code != http.StatusOK {
		t.Fatalf("manifest update status = %d: %s", update.Code, update.Body.String())
	}
	updated := requireActionDocument(t, update.Body.Bytes())
	if updated["name"] != name || updated["title"] != "Updated through action" {
		t.Fatalf("manifest update response = %#v", updated)
	}

	var auditCount, outboxCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, name).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, name).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 2 || outboxCount != 2 {
		t.Fatalf("manifest action side effects = audit:%d outbox:%d; want two audits and two change events", auditCount, outboxCount)
	}
}

func TestPOSCreateTransactionPreservesSaleAndPaymentContract(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, db, site, reg)
	registerPOSIntegrationView(reg)

	checkout := map[string]any{
		"reference": "POS-CHECKOUT-1", "cart": []any{map[string]any{"product": "PROD-001", "rate": 125.0, "quantity": 2}},
		"payment_method": "cash", "total": 250.0,
	}
	result := serveManifestAction(t, handler, site, db, reg, "complete", checkout)
	if result.Code != http.StatusOK {
		t.Fatalf("POS transaction status = %d: %s", result.Code, result.Body.String())
	}
	sale := requireActionDocument(t, result.Body.Bytes())
	saleName, _ := sale["name"].(string)
	if saleName == "" || sale["reference"] == "" || sale["total"] != float64(250) {
		t.Fatalf("POS sale response lost generated reference or calculated total: %#v", sale)
	}
	if _, leaked := sale["items"]; leaked {
		t.Fatalf("POS transaction response changed legacy child-row projection: %#v", sale)
	}
	var itemCount int
	var product string
	var quantity, unitPrice float64
	if err := db.QueryRow("SELECT COUNT(*), MAX(product), MAX(quantity), MAX(unit_price) FROM `tabSale__items` WHERE parent = ?", saleName).Scan(&itemCount, &product, &quantity, &unitPrice); err != nil {
		t.Fatal("query POS sale items:", err)
	}
	if itemCount != 1 || product != "PROD-001" || quantity != 2 || unitPrice != 125 {
		t.Fatalf("POS sale item persistence count=%d product=%q quantity=%v unit_price=%v", itemCount, product, quantity, unitPrice)
	}
	var paymentCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabPayment` WHERE sale = ? AND method = ? AND amount = ? AND status = ?", saleName, "cash", 250, "Draft").Scan(&paymentCount); err != nil {
		t.Fatal("query POS payment:", err)
	}
	if paymentCount != 1 {
		var saleValue, methodValue, statusValue string
		var amountValue float64
		_ = db.QueryRow("SELECT COALESCE(sale,''), COALESCE(method,''), COALESCE(status,''), COALESCE(amount,0) FROM `tabPayment` LIMIT 1").Scan(&saleValue, &methodValue, &statusValue, &amountValue)
		t.Fatalf("POS bundled payment initial-state count = %d, want 1 (sale=%q method=%q status=%q amount=%v)", paymentCount, saleValue, methodValue, statusValue, amountValue)
	}
	replayed := serveManifestAction(t, handler, site, db, reg, "complete", checkout)
	if replayed.Code != http.StatusOK || replayed.Header().Get("X-Kora-Replay") != "true" {
		t.Fatalf("POS retry did not replay original result: status=%d replay=%q body=%s", replayed.Code, replayed.Header().Get("X-Kora-Replay"), replayed.Body.String())
	}
	replayDoc := requireActionDocument(t, replayed.Body.Bytes())
	if replayDoc["name"] != saleName {
		t.Fatalf("POS retry returned %v, want original Sale %q", replayDoc["name"], saleName)
	}
	var saleCount, auditCount, outboxCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabSale`").Scan(&saleCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND operation_id = (SELECT operation_id FROM _kora_operation_audit WHERE site = ? AND doctype = 'Sale' AND doc_name = ? ORDER BY created_at DESC LIMIT 1)", site, site, saleName).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, saleName).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if saleCount != 1 || auditCount != 2 || outboxCount != 1 {
		t.Fatalf("POS retry duplicated or lost effects: sales=%d operation_audit=%d sale_outbox=%d", saleCount, auditCount, outboxCount)
	}
}

func TestPOSCreateTransactionRollsBackSaleWhenPaymentWriteFails(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, db, site, reg)
	registerPOSIntegrationView(reg)
	if _, err := db.Exec("CREATE TRIGGER reject_pos_payment BEFORE INSERT ON `tabPayment` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'simulated payment ledger failure'"); err != nil {
		t.Fatal("install payment failure trigger:", err)
	}
	result := serveManifestAction(t, handler, site, db, reg, "complete", map[string]any{
		"cart":           []any{map[string]any{"product": "PROD-001", "rate": 125.0, "quantity": 2}},
		"payment_method": "cash", "total": 250.0,
	})
	if result.Code == http.StatusOK {
		t.Fatalf("POS transaction succeeded despite payment persistence failure: %s", result.Body.String())
	}
	var saleCount, childCount, paymentCount int
	queries := []struct {
		query string
		count *int
	}{
		{query: "SELECT COUNT(*) FROM `tabSale`", count: &saleCount},
		{query: "SELECT COUNT(*) FROM `tabSale__items`", count: &childCount},
		{query: "SELECT COUNT(*) FROM `tabPayment`", count: &paymentCount},
	}
	for _, check := range queries {
		if err := db.QueryRow(check.query).Scan(check.count); err != nil {
			t.Fatalf("verify rollback with %q: %v", check.query, err)
		}
	}
	if saleCount != 0 || childCount != 0 || paymentCount != 0 {
		t.Fatalf("failed POS transaction left partial rows: sale=%d items=%d payment=%d", saleCount, childCount, paymentCount)
	}
}

func TestConfirmedChannelMutationUsesKernelReceiptAndPreservesResponse(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	for _, statement := range kdb.ExtensibilityTablesMySQL() {
		if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE IF NOT EXISTS _kora_channel_audit") {
			if _, err := database.Exec(statement); err != nil {
				t.Fatalf("create channel audit table: %v", err)
			}
		}
	}

	invoke := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/channel/tools/mutate", strings.NewReader(`{"tool_name":"testdoc_create","args":{"title":"channel retry"}}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Request.Header.Set("X-Kora-Confirm", "confirmed")
		ctx.Request.Header.Set("Idempotency-Key", "channel-test-retry-0001")
		ctx.Set("site_name", site)
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_db_type", "mysql")
		ctx.Set("auth_type", "channel_session")
		ctx.Set("channel_permissions", []doctype.Permission{{Doctype: "TestDoc", Create: true}})
		ctx.Set("channel_sender_address", "+254700000001")
		ctx.Set("channel_session_id", "channel-session-1")
		ctx.Set("channel_conversation_key", "channel-conversation-1")
		ctx.Set("user", "alice@example.test")
		ctx.Set("policy_execution", integrationTestExecution(t, site, "alice@example.test", []string{doctype.AdminRole}))
		handler.HandleChannelMutate(ctx)
		return recorder
	}

	first := invoke()
	if first.Code != http.StatusOK {
		t.Fatalf("confirmed channel create returned HTTP %d: %s", first.Code, first.Body.String())
	}
	var firstBody struct {
		Data channelToolResponse `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode channel create response: %v", err)
	}
	if !strings.HasPrefix(firstBody.Data.Result, `Created TestDoc "`) {
		t.Fatalf("channel create did not execute after explicit confirmation: %q", firstBody.Data.Result)
	}

	replay := invoke()
	if replay.Code != http.StatusOK {
		t.Fatalf("channel retry returned HTTP %d: %s", replay.Code, replay.Body.String())
	}
	var replayBody struct {
		Data channelToolResponse `json:"data"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayBody); err != nil {
		t.Fatalf("decode channel retry response: %v", err)
	}
	if replayBody.Data.Result != firstBody.Data.Result {
		t.Fatalf("channel retry response changed: first=%q replay=%q", firstBody.Data.Result, replayBody.Data.Result)
	}
	for table, want := range map[string]int{"tabTestDoc": 1, "_kora_operation_audit": 1, "_kora_outbox": 1, "_kora_idempotency_receipt": 1, "_kora_channel_audit": 2} {
		var count int
		query := "SELECT COUNT(*) FROM `" + table + "`"
		if table != "tabTestDoc" && table != "_kora_channel_audit" {
			query += " WHERE site = ?"
			if err := database.QueryRow(query, site).Scan(&count); err != nil {
				t.Fatalf("count %s rows: %v", table, err)
			}
		} else if err := database.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
}

func TestExternalOperationActionPreservesInitiationAndRetryContract(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, db, site, reg)
	reg.Views.Register(&doctype.View{
		Name: "Payment initiation", Route: "/payment", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "initiate", Trigger: "on_click", Type: "initiate_external_operation",
			Config: map[string]any{"operation_type": "Payment", "purpose": "POS sale payment", "source_doctype": "Sale", "provider": "M-Pesa"},
		}}}},
	})
	request := map[string]any{
		"client_reference": "pos-test-operation-1", "total": 250.0,
		"payment_method": "mpesa", "customer_phone": "+254700000001",
	}
	first := serveManifestAction(t, handler, site, db, reg, "initiate", request)
	if first.Code != http.StatusOK {
		t.Fatalf("initiate operation status = %d: %s", first.Code, first.Body.String())
	}
	operation := requireActionDocument(t, first.Body.Bytes())
	operationName, _ := operation["name"].(string)
	if operationName == "" || operation["status"] != "Pending" || operation["idempotency_key"] != "pos-test-operation-1" {
		t.Fatalf("operation initiation result = %#v", operation)
	}
	if operation["initiated_at"] == nil {
		t.Fatalf("operation initiation omitted its transaction-time default: %#v", operation["initiated_at"])
	}
	var eventTimestamps int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND received_at IS NOT NULL AND processed_at IS NOT NULL", operationName).Scan(&eventTimestamps); err != nil {
		t.Fatalf("query initiation event timestamps: %v", err)
	}
	if eventTimestamps != 1 {
		t.Fatalf("initiation event timestamp rows=%d, want one", eventTimestamps)
	}
	var paymentCount, eventCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabPayment` WHERE external_operation = ? AND status = ? AND amount = ?", operationName, "Pending", 250).Scan(&paymentCount); err != nil {
		t.Fatal("query initiated payment:", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = ?", operationName, "Initiate").Scan(&eventCount); err != nil {
		t.Fatal("query operation event:", err)
	}
	if paymentCount != 1 || eventCount != 1 {
		t.Fatalf("operation initiation side effects payment=%d event=%d; want one each", paymentCount, eventCount)
	}
	retry := serveManifestAction(t, handler, site, db, reg, "initiate", request)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry operation status = %d: %s", retry.Code, retry.Body.String())
	}
	retryDoc := requireActionDocument(t, retry.Body.Bytes())
	if retryDoc["name"] != operationName {
		t.Fatalf("retry returned operation %v; want %q", retryDoc["name"], operationName)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabPayment` WHERE external_operation = ?", operationName).Scan(&paymentCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ?", operationName).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if paymentCount != 1 || eventCount != 1 {
		t.Fatalf("operation retry duplicated side effects payment=%d event=%d", paymentCount, eventCount)
	}
}

func TestExternalOperationInitiationEventFailureIsAtomic(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)
	registry.Views.Register(&doctype.View{
		Name: "Payment initiation failure", Route: "/payment-initiation-failure", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "initiate", Trigger: "on_click", Type: "initiate_external_operation",
			Config: map[string]any{"operation_type": "Payment", "purpose": "POS sale payment", "source_doctype": "Sale", "provider": "M-Pesa"},
		}}}},
	})
	if _, err := database.Exec("CREATE TRIGGER fail_external_operation_event BEFORE INSERT ON `tabExternal Operation Event` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'forced initiation event failure'"); err != nil {
		t.Fatalf("install initiation event failure trigger: %v", err)
	}
	t.Cleanup(func() { _, _ = database.Exec("DROP TRIGGER IF EXISTS fail_external_operation_event") })
	request := map[string]any{
		"client_reference": "pos-test-initiation-event-failure", "total": 250.0,
		"payment_method": "mpesa", "customer_phone": "+254700000001",
	}
	response := serveManifestAction(t, handler, site, database, registry, "initiate", request)
	if response.Code == http.StatusOK {
		t.Fatalf("initiation returned success although its durable event write failed: %s", response.Body.String())
	}
	for table, query := range map[string]string{
		"operation": "SELECT COUNT(*) FROM `tabExternal Operation`",
		"payment":   "SELECT COUNT(*) FROM `tabPayment`",
		"event":     "SELECT COUNT(*) FROM `tabExternal Operation Event`",
		"audit":     "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND status = 'completed'",
		"outbox":    "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?",
		"receipt":   "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.mutate_bundle'",
	} {
		var count int
		args := []any(nil)
		if table == "audit" || table == "outbox" || table == "receipt" {
			args = []any{site}
		}
		if err := database.QueryRow(query, args...).Scan(&count); err != nil {
			t.Fatalf("count %s after initiation event failure: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("failed initiation left %d %s rows", count, table)
		}
	}
}

func TestExternalOperationProviderSuccessCommitsPaymentAndEventTogether(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)
	for _, statement := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	store := &script.Store{DB: database, Dialect: kdb.Resolve("mysql")}
	if err := store.Insert(script.ScriptRecord{
		Name: "successful-payment-initiation", Site: site, ScriptType: script.TypeAPIMethod,
		DocType: "External Operation", Event: script.EventPayment, IsActive: true,
	}); err != nil {
		t.Fatalf("insert operation script: %v", err)
	}
	handler.SiteScriptStores = map[string]*script.Store{site: store}
	handler.ScriptRunner = fixedOperationScriptRunner{result: map[string]any{
		"status": "Succeeded", "provider_reference": "RCP-SUCCESS-1", "response_payload": map[string]any{"accepted": true},
	}}
	registry.Views.Register(&doctype.View{
		Name: "Payment initiation success", Route: "/payment-initiation-success", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "initiate", Trigger: "on_click", Type: "initiate_external_operation",
			Config: map[string]any{"operation_type": "Payment", "purpose": "POS sale payment", "source_doctype": "Sale", "provider": "M-Pesa", "script": "successful-payment-initiation"},
		}}}},
	})
	request := map[string]any{
		"client_reference": "pos-test-initiation-success", "total": 250.0,
		"payment_method": "mpesa", "customer_phone": "+254700000001",
	}
	response := serveManifestAction(t, handler, site, database, registry, "initiate", request)
	if response.Code != http.StatusOK {
		t.Fatalf("successful provider initiation returned HTTP %d: %s", response.Code, response.Body.String())
	}
	operation := requireActionDocument(t, response.Body.Bytes())
	operationName, _ := operation["name"].(string)
	if operationName == "" || operation["status"] != "Succeeded" || operation["provider_reference"] != "RCP-SUCCESS-1" {
		t.Fatalf("successful provider operation = %#v", operation)
	}
	var paymentStatus, paymentReference, eventStatus string
	if err := database.QueryRow("SELECT status, provider_reference FROM `tabPayment` WHERE external_operation = ?", operationName).Scan(&paymentStatus, &paymentReference); err != nil {
		t.Fatalf("read successful payment: %v", err)
	}
	if err := database.QueryRow("SELECT new_status FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = 'Initiate' ORDER BY creation DESC LIMIT 1", operationName).Scan(&eventStatus); err != nil {
		t.Fatalf("read successful operation event: %v", err)
	}
	if paymentStatus != "Succeeded" || paymentReference != "RCP-SUCCESS-1" || eventStatus != "Succeeded" {
		t.Fatalf("provider result split across operation/payment/event: payment=(%s,%s) event=%s", paymentStatus, paymentReference, eventStatus)
	}
	if _, err := database.Exec("CREATE TRIGGER fail_success_operation_event BEFORE INSERT ON `tabExternal Operation Event` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'forced successful outcome event failure'"); err != nil {
		t.Fatalf("install provider outcome event failure trigger: %v", err)
	}
	t.Cleanup(func() { _, _ = database.Exec("DROP TRIGGER IF EXISTS fail_success_operation_event") })
	failingRequest := map[string]any{
		"client_reference": "pos-test-initiation-success-event-failure", "total": 250.0,
		"payment_method": "mpesa", "customer_phone": "+254700000001",
	}
	var receiptsBeforeOutcome int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.mutate_bundle'", site).Scan(&receiptsBeforeOutcome); err != nil {
		t.Fatalf("count receipts before failed provider outcome: %v", err)
	}
	failingResponse := serveManifestAction(t, handler, site, database, registry, "initiate", failingRequest)
	if failingResponse.Code == http.StatusOK {
		t.Fatalf("provider success returned HTTP success although its result event failed: %s", failingResponse.Body.String())
	}
	var failedOperationName, failedOperationStatus, failedPaymentStatus string
	if err := database.QueryRow("SELECT name, status FROM `tabExternal Operation` WHERE idempotency_key = ?", failingRequest["client_reference"]).Scan(&failedOperationName, &failedOperationStatus); err != nil {
		t.Fatalf("read operation after failed outcome bundle: %v", err)
	}
	if err := database.QueryRow("SELECT status FROM `tabPayment` WHERE external_operation = ?", failedOperationName).Scan(&failedPaymentStatus); err != nil {
		t.Fatalf("read payment after failed outcome bundle: %v", err)
	}
	var failedEvents, completedOperationAudits, outcomeReceipts int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ?", failedOperationName).Scan(&failedEvents); err != nil {
		t.Fatalf("count failed outcome events: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doctype = 'External Operation' AND doc_name = ? AND status = 'completed'", site, failedOperationName).Scan(&completedOperationAudits); err != nil {
		t.Fatalf("count failed outcome operation audits: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.mutate_bundle'", site).Scan(&outcomeReceipts); err != nil {
		t.Fatalf("count failed outcome receipts: %v", err)
	}
	if failedOperationStatus != "Initiating" || failedPaymentStatus != "Pending" || failedEvents != 0 || completedOperationAudits != 1 || outcomeReceipts != receiptsBeforeOutcome+1 {
		t.Fatalf("failed outcome bundle left operation=%s payment=%s events=%d operation_audits=%d receipts=%d (before=%d; only the initiating receipt should be added)", failedOperationStatus, failedPaymentStatus, failedEvents, completedOperationAudits, outcomeReceipts, receiptsBeforeOutcome)
	}
}

type fixedOperationScriptRunner struct {
	result map[string]any
}

func (r fixedOperationScriptRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return &script.ExecuteResult{Result: r.result}, nil
}

func (fixedOperationScriptRunner) Validate(string) error { return nil }
func (fixedOperationScriptRunner) Close() error          { return nil }

func TestExternalOperationProviderFailureKeepsPaymentAndEventConsistent(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)
	for _, statement := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	store := &script.Store{DB: database, Dialect: kdb.Resolve("mysql")}
	if err := store.Insert(script.ScriptRecord{
		Name: "fail-payment-initiation", Site: site, ScriptType: script.TypeAPIMethod,
		DocType: "External Operation", Event: script.EventPayment, IsActive: true,
	}); err != nil {
		t.Fatalf("insert provider script: %v", err)
	}
	handler.SiteScriptStores = map[string]*script.Store{site: store}
	handler.ScriptRunner = failingOperationScriptRunner{}
	registry.Views.Register(&doctype.View{
		Name: "Payment initiation failure", Route: "/payment-failure", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "initiate-failing", Trigger: "on_click", Type: "initiate_external_operation",
			Config: map[string]any{"operation_type": "Payment", "purpose": "POS sale payment", "source_doctype": "Sale", "provider": "M-Pesa", "script": "fail-payment-initiation"},
		}}}},
	})
	request := map[string]any{"client_reference": "pos-test-operation-failure", "total": 250.0, "payment_method": "mpesa", "customer_phone": "+254700000001"}
	response := serveManifestAction(t, handler, site, database, registry, "initiate-failing", request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "operation.failed") {
		t.Fatalf("provider failure response = %d: %s", response.Code, response.Body.String())
	}
	var operationName string
	if err := database.QueryRow("SELECT name FROM `tabExternal Operation` WHERE idempotency_key = ?", request["client_reference"]).Scan(&operationName); err != nil {
		t.Fatal("load failed operation:", err)
	}
	var operationStatus, operationError, paymentStatus string
	if err := database.QueryRow("SELECT status, error_message FROM `tabExternal Operation` WHERE name = ?", operationName).Scan(&operationStatus, &operationError); err != nil {
		t.Fatal("read failed operation:", err)
	}
	if err := database.QueryRow("SELECT status FROM `tabPayment` WHERE external_operation = ?", operationName).Scan(&paymentStatus); err != nil {
		t.Fatal("read failed payment:", err)
	}
	var eventCount, operationAudits, paymentAudits, operationEvents, totalOutbox int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = 'Initiate' AND processing_status = 'Failed'", operationName).Scan(&eventCount); err != nil {
		t.Fatal("count failure events:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, operationName).Scan(&operationAudits); err != nil {
		t.Fatal("count operation audits:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doctype = 'Payment' AND doc_name = (SELECT name FROM `tabPayment` WHERE external_operation = ?) AND status = 'completed'", site, operationName).Scan(&paymentAudits); err != nil {
		t.Fatal("count payment audits:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id IN (?, (SELECT name FROM `tabPayment` WHERE external_operation = ?))", site, operationName, operationName).Scan(&operationEvents); err != nil {
		t.Fatal("count operation/payment outbox events:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", site).Scan(&totalOutbox); err != nil {
		t.Fatal("count total outbox events:", err)
	}
	if operationStatus != "Failed" || paymentStatus != "Failed" || operationError == "" || eventCount != 1 || operationAudits != 2 || paymentAudits != 2 || operationEvents != 4 || totalOutbox != 5 {
		t.Fatalf("failed provider state operation=(%q,%q) payment=%q failure_events=%d operation_audits=%d payment_audits=%d record_outbox=%d total_outbox=%d; want Failed/Failed with error, 1 event, 2/2 audits and 4/5 outbox, one change event per mutation", operationStatus, operationError, paymentStatus, eventCount, operationAudits, paymentAudits, operationEvents, totalOutbox)
	}

	if _, err := database.Exec("CREATE TRIGGER reject_operation_failure_event BEFORE INSERT ON `tabExternal Operation Event` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'simulated event write failure'"); err != nil {
		t.Fatalf("install event failure trigger: %v", err)
	}
	secondRequest := map[string]any{"client_reference": "pos-test-operation-event-failure", "total": 125.0, "payment_method": "mpesa", "customer_phone": "+254700000002"}
	failedPersistence := serveManifestAction(t, handler, site, database, registry, "initiate-failing", secondRequest)
	if failedPersistence.Code != http.StatusServiceUnavailable {
		t.Fatalf("failure-event persistence response = %d, want 503 for dependency unavailability: %s", failedPersistence.Code, failedPersistence.Body.String())
	}
	if _, err := database.Exec("DROP TRIGGER reject_operation_failure_event"); err != nil {
		t.Fatalf("remove event failure trigger: %v", err)
	}
	var secondOperationName string
	if err := database.QueryRow("SELECT name FROM `tabExternal Operation` WHERE idempotency_key = ?", secondRequest["client_reference"]).Scan(&secondOperationName); err != nil {
		t.Fatal("load operation after event write failure:", err)
	}
	if err := database.QueryRow("SELECT status FROM `tabExternal Operation` WHERE name = ?", secondOperationName).Scan(&operationStatus); err != nil {
		t.Fatal("read operation after event write failure:", err)
	}
	if err := database.QueryRow("SELECT status FROM `tabPayment` WHERE external_operation = ?", secondOperationName).Scan(&paymentStatus); err != nil {
		t.Fatal("read payment after event write failure:", err)
	}
	if operationStatus != "Initiating" || paymentStatus != "Pending" {
		t.Fatalf("event-write rollback changed initial operation/payment state: operation=%q payment=%q; want Initiating/Pending", operationStatus, paymentStatus)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ?", secondOperationName).Scan(&eventCount); err != nil {
		t.Fatal("count event after write failure:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND operation_id = (SELECT operation_id FROM _kora_operation_audit WHERE site = ? AND doctype = 'External Operation' AND doc_name = ? ORDER BY created_at LIMIT 1) AND status = 'completed'", site, site, secondOperationName).Scan(&operationAudits); err != nil {
		t.Fatal("count operation bundle audit after event failure:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id IN (?, (SELECT name FROM `tabPayment` WHERE external_operation = ?))", site, secondOperationName, secondOperationName).Scan(&operationEvents); err != nil {
		t.Fatal("count operation bundle outbox after event failure:", err)
	}
	if eventCount != 0 || operationAudits != 2 || operationEvents != 4 {
		t.Fatalf("event-write rollback left events=%d audits=%d record_outbox=%d; want 0/2/4 (initial operation events plus record-created events only)", eventCount, operationAudits, operationEvents)
	}
}

type failingOperationScriptRunner struct{}

func (failingOperationScriptRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return nil, errors.New("simulated provider failure")
}

func (failingOperationScriptRunner) Validate(string) error { return nil }
func (failingOperationScriptRunner) Close() error          { return nil }

func TestMPesaCallbackDoesNotSplitOperationAndPaymentOnWriteFailure(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, db, site, reg)
	if _, err := db.Exec("INSERT INTO `tabExternal Operation` (name, operation_type, purpose, source_doctype, provider, status, provider_request_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"OP-1", "Payment", "POS payment", "Sale", "M-Pesa", "Pending", "checkout-1"); err != nil {
		t.Fatal("seed external operation:", err)
	}
	if _, err := db.Exec("INSERT INTO `tabSales Invoice` (name, title, total_amount) VALUES (?, ?, ?)", "INV-OP-1", "M-Pesa failure fixture", 250); err != nil {
		t.Fatal("seed Sales Invoice:", err)
	}
	if _, err := db.Exec("INSERT INTO `tabPayment` (name, sales_invoice, reference, external_operation, status, method, amount) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"PAY-1", "INV-OP-1", "PAY-OP-1", "OP-1", "Pending", "mpesa", 250); err != nil {
		t.Fatal("seed Payment:", err)
	}
	if _, err := db.Exec("CREATE TRIGGER reject_mpesa_payment BEFORE UPDATE ON `tabPayment` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'simulated payment update failure'"); err != nil {
		t.Fatal("install Payment failure trigger:", err)
	}

	body := `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-1","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"RCP-1"}]}}}}`
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/mpesa/stk/test-callback-token", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	configureMPesaCallbackTestToken(t, db, site, kdb.Resolve("mysql"))
	bindMPesaCallbackTestIdentity(ctx, site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	handler.HandleMPesaSTKCallback(ctx)
	if recorder.Code == http.StatusOK {
		t.Fatalf("callback reported success despite Payment update failure: %s", recorder.Body.String())
	}
	var operationStatus, paymentStatus string
	if err := db.QueryRow("SELECT status FROM `tabExternal Operation` WHERE name = ?", "OP-1").Scan(&operationStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status FROM `tabPayment` WHERE name = ?", "PAY-1").Scan(&paymentStatus); err != nil {
		t.Fatal(err)
	}
	var callbackEvents int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = ?", "OP-1", "Callback").Scan(&callbackEvents); err != nil {
		t.Fatal(err)
	}
	if operationStatus != "Pending" || paymentStatus != "Pending" || callbackEvents != 0 {
		t.Fatalf("failed callback partially committed: operation=%q payment=%q events=%d", operationStatus, paymentStatus, callbackEvents)
	}
}

func TestMPesaCallbackRejectsWrongTenantTokenBeforeMutation(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)
	configureMPesaCallbackTestToken(t, database, site, kdb.Resolve("mysql"))
	body := `{"Body":{"stkCallback":{"CheckoutRequestID":"unknown-checkout","ResultCode":0,"ResultDesc":"Success"}}}`
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/mpesa/stk/wrong-token", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "token", Value: "wrong-token"}}
	ctx.Set("site_name", site)
	ctx.Set("site_id", site)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_db_type", "mysql")
	handler.HandleMPesaSTKCallback(ctx)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("callback with wrong tenant token returned %d, want 401: %s", recorder.Code, recorder.Body.String())
	}
	var audits, events int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ?", site).Scan(&audits); err != nil {
		t.Fatal("count operation audit after rejected callback:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", site).Scan(&events); err != nil {
		t.Fatal("count outbox after rejected callback:", err)
	}
	if audits != 0 || events != 0 {
		t.Fatalf("rejected callback wrote audit=%d outbox=%d; want none", audits, events)
	}
}

func TestMPesaCallbackCommitsAtomicallyAndTreatsTerminalRetryAsDuplicate(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, db, site, reg)
	if _, err := db.Exec("INSERT INTO `tabExternal Operation` (name, operation_type, purpose, source_doctype, provider, status, provider_request_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"OP-2", "Payment", "POS payment", "Sale", "M-Pesa", "Pending", "checkout-2"); err != nil {
		t.Fatal("seed external operation:", err)
	}
	if _, err := db.Exec("INSERT INTO `tabSales Invoice` (name, title, total_amount) VALUES (?, ?, ?)", "INV-OP-2", "M-Pesa success fixture", 250); err != nil {
		t.Fatal("seed Sales Invoice:", err)
	}
	if _, err := db.Exec("INSERT INTO `tabPayment` (name, sales_invoice, reference, external_operation, status, method, amount) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"PAY-2", "INV-OP-2", "PAY-OP-2", "OP-2", "Pending", "mpesa", 250); err != nil {
		t.Fatal("seed Payment:", err)
	}
	body := `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-2","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"RCP-2"}]}}}}`
	first := serveMPesaCallback(t, handler, db, reg, site, body)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"payment_status":"Succeeded"`) {
		t.Fatalf("success callback response = %d: %s", first.Code, first.Body.String())
	}
	var operationStatus, providerReference, paymentStatus, paymentReference string
	if err := db.QueryRow("SELECT status, provider_reference FROM `tabExternal Operation` WHERE name = ?", "OP-2").Scan(&operationStatus, &providerReference); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT status, provider_reference FROM `tabPayment` WHERE name = ?", "PAY-2").Scan(&paymentStatus, &paymentReference); err != nil {
		t.Fatal(err)
	}
	if operationStatus != "Succeeded" || providerReference != "RCP-2" || paymentStatus != "Succeeded" || paymentReference != "RCP-2" {
		t.Fatalf("callback state operation=(%s,%s) payment=(%s,%s)", operationStatus, providerReference, paymentStatus, paymentReference)
	}
	var firstEventCount, operationAuditCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = ? AND new_status = ?", "OP-2", "Callback", "Succeeded").Scan(&firstEventCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND operation_id = (SELECT operation_id FROM _kora_operation_audit WHERE site = ? AND doctype = 'External Operation' AND doc_name = ? ORDER BY created_at LIMIT 1)", site, site, "OP-2").Scan(&operationAuditCount); err != nil {
		t.Fatal(err)
	}
	if firstEventCount != 1 || operationAuditCount != 3 {
		t.Fatalf("callback bundle effects event=%d audit=%d; want 1/3", firstEventCount, operationAuditCount)
	}

	retry := serveMPesaCallback(t, handler, db, reg, site, body)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("terminal callback retry = %d: %s", retry.Code, retry.Body.String())
	}
	var eventCount, paymentCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = ?", "OP-2", "Callback").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabPayment` WHERE name = ? AND status = ?", "PAY-2", "Succeeded").Scan(&paymentCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 2 || paymentCount != 1 {
		t.Fatalf("duplicate callback state events=%d successful payment rows=%d; want 2/1", eventCount, paymentCount)
	}
}

func TestMPesaConcurrentCallbacksHaveOneTerminalTransition(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)
	if _, err := database.Exec("INSERT INTO `tabExternal Operation` (name, operation_type, purpose, source_doctype, provider, status, provider_request_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"OP-CONCURRENT", "Payment", "POS payment", "Sale", "M-Pesa", "Pending", "checkout-concurrent"); err != nil {
		t.Fatal("seed external operation:", err)
	}
	if _, err := database.Exec("INSERT INTO `tabPayment` (name, reference, external_operation, status, method, amount) VALUES (?, ?, ?, ?, ?, ?)",
		"PAY-CONCURRENT", "PAY-REF-CONCURRENT", "OP-CONCURRENT", "Pending", "mpesa", 250); err != nil {
		t.Fatal("seed payment:", err)
	}
	body := `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-concurrent","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"RCP-CONCURRENT"}]}}}}`
	configureMPesaCallbackTestToken(t, database, site, kdb.Resolve("mysql"))
	gin.SetMode(gin.TestMode)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/mpesa/stk/test-callback-token", strings.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set("site_name", site)
			bindMPesaCallbackTestIdentity(ctx, site)
			ctx.Set("site_db", database)
			ctx.Set("site_registry", registry)
			ctx.Set("site_db_type", "mysql")
			handler.HandleMPesaSTKCallback(ctx)
			responses <- recorder
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		response := <-responses
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent callback returned HTTP %d: %s", response.Code, response.Body.String())
		}
	}
	var operationStatus, paymentStatus, paymentReference string
	if err := database.QueryRow("SELECT status FROM `tabExternal Operation` WHERE name = ?", "OP-CONCURRENT").Scan(&operationStatus); err != nil {
		t.Fatal("read operation:", err)
	}
	if err := database.QueryRow("SELECT status, provider_reference FROM `tabPayment` WHERE name = ?", "PAY-CONCURRENT").Scan(&paymentStatus, &paymentReference); err != nil {
		t.Fatal("read payment:", err)
	}
	var successEvents, callbackEvents, successfulAudits int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND new_status = 'Succeeded' AND processing_status = 'Processed'", "OP-CONCURRENT").Scan(&successEvents); err != nil {
		t.Fatal("count successful callback events:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = 'Callback'", "OP-CONCURRENT").Scan(&callbackEvents); err != nil {
		t.Fatal("count callback events:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doctype = 'External Operation' AND doc_name = ? AND status = 'completed'", site, "OP-CONCURRENT").Scan(&successfulAudits); err != nil {
		t.Fatal("count operation audits:", err)
	}
	if operationStatus != "Succeeded" || paymentStatus != "Succeeded" || paymentReference != "RCP-CONCURRENT" || successEvents != 1 || callbackEvents != 2 || successfulAudits != 1 {
		t.Fatalf("concurrent callback state operation=%q payment=(%q,%q) success_events=%d callback_events=%d operation_audits=%d; want Succeeded/Succeeded+receipt/1/2/1", operationStatus, paymentStatus, paymentReference, successEvents, callbackEvents, successfulAudits)
	}
}

func TestMPesaTerminalCallbackWithConflictingResultIsIgnored(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)

	tests := []struct {
		name, operationName, checkoutID, status, receipt, callback string
	}{
		{
			name: "success cannot replace failed terminal operation", operationName: "OP-TERMINAL-FAILED",
			checkoutID: "checkout-terminal-failed", status: "Failed", callback: `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-terminal-failed","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"LATE-SUCCESS"}]}}}}`,
		},
		{
			name: "different success receipt is not a duplicate", operationName: "OP-TERMINAL-RECEIPT",
			checkoutID: "checkout-terminal-receipt", status: "Succeeded", receipt: "ORIGINAL-RECEIPT", callback: `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-terminal-receipt","ResultCode":0,"ResultDesc":"Success","CallbackMetadata":{"Item":[{"Name":"MpesaReceiptNumber","Value":"OTHER-RECEIPT"}]}}}}`,
		},
		{
			name: "failure cannot replace successful terminal operation", operationName: "OP-TERMINAL-SUCCEEDED",
			checkoutID: "checkout-terminal-succeeded", status: "Succeeded", receipt: "SUCCESS-RECEIPT", callback: `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-terminal-succeeded","ResultCode":1032,"ResultDesc":"late failure"}}}`,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paymentName := fmt.Sprintf("PAY-TERMINAL-%d", index)
			if _, err := database.Exec("INSERT INTO `tabExternal Operation` (name, operation_type, purpose, source_doctype, provider, status, provider_request_id, provider_reference) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
				test.operationName, "Payment", "POS payment", "Sale", "M-Pesa", test.status, test.checkoutID, test.receipt); err != nil {
				t.Fatal("seed terminal external operation:", err)
			}
			if _, err := database.Exec("INSERT INTO `tabPayment` (name, reference, external_operation, status, method, amount, provider_reference) VALUES (?, ?, ?, ?, ?, ?, ?)",
				paymentName, "REF-"+paymentName, test.operationName, test.status, "mpesa", 250, test.receipt); err != nil {
				t.Fatal("seed terminal payment:", err)
			}

			response := serveMPesaCallback(t, handler, database, registry, site, test.callback)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ignored"`) {
				t.Fatalf("conflicting terminal callback response = %d: %s; want acknowledged and ignored", response.Code, response.Body.String())
			}
			var operationStatus, operationReceipt, paymentStatus, paymentReceipt string
			if err := database.QueryRow("SELECT status, provider_reference FROM `tabExternal Operation` WHERE name = ?", test.operationName).Scan(&operationStatus, &operationReceipt); err != nil {
				t.Fatal("read terminal operation:", err)
			}
			if err := database.QueryRow("SELECT status, provider_reference FROM `tabPayment` WHERE name = ?", paymentName).Scan(&paymentStatus, &paymentReceipt); err != nil {
				t.Fatal("read terminal payment:", err)
			}
			if operationStatus != test.status || operationReceipt != test.receipt || paymentStatus != test.status || paymentReceipt != test.receipt {
				t.Fatalf("conflicting callback changed terminal state: operation=(%s,%s) payment=(%s,%s)", operationStatus, operationReceipt, paymentStatus, paymentReceipt)
			}
			var ignoredEvents int
			if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = 'Callback' AND processing_status = 'Ignored'", test.operationName).Scan(&ignoredEvents); err != nil {
				t.Fatal("count ignored callback events:", err)
			}
			if ignoredEvents != 1 {
				t.Fatalf("ignored callback events=%d, want 1", ignoredEvents)
			}
		})
	}
}

func TestMPesaCallbackRejectsMalformedAndUnknownRequests(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)

	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "malformed JSON", body: `{`, want: http.StatusBadRequest},
		{name: "missing checkout request ID", body: `{"Body":{"stkCallback":{"ResultCode":0}}}`, want: http.StatusBadRequest},
		{name: "unknown checkout request ID", body: `{"Body":{"stkCallback":{"CheckoutRequestID":"not-found","ResultCode":0}}}`, want: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveMPesaCallback(t, handler, database, registry, site, test.body)
			if response.Code != test.want {
				t.Fatalf("callback status=%d body=%s; want %d", response.Code, response.Body.String(), test.want)
			}
		})
	}
	var operationEvents, completedAudits, receipts int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE event_type = 'Callback'").Scan(&operationEvents); err != nil {
		t.Fatal("count callback events:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.mutate_bundle' AND status = 'completed'", site).Scan(&completedAudits); err != nil {
		t.Fatal("count completed callback bundles:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ?", site).Scan(&receipts); err != nil {
		t.Fatal("count callback receipts:", err)
	}
	if operationEvents != 0 || completedAudits != 0 || receipts != 0 {
		t.Fatalf("rejected callbacks wrote side effects: events=%d audits=%d receipts=%d", operationEvents, completedAudits, receipts)
	}
}

func TestMPesaFailureCallbackAndTerminalRetry(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registerPOSIntegrationTypes(t, database, site, registry)
	if _, err := database.Exec("INSERT INTO `tabExternal Operation` (name, operation_type, purpose, source_doctype, provider, status, provider_request_id) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"OP-FAILURE", "Payment", "POS payment", "Sale", "M-Pesa", "Pending", "checkout-failure"); err != nil {
		t.Fatal("seed external operation:", err)
	}
	if _, err := database.Exec("INSERT INTO `tabPayment` (name, reference, external_operation, status, method, amount) VALUES (?, ?, ?, ?, ?, ?)",
		"PAY-FAILURE", "PAY-REF-FAILURE", "OP-FAILURE", "Pending", "mpesa", 250); err != nil {
		t.Fatal("seed payment:", err)
	}
	body := `{"Body":{"stkCallback":{"CheckoutRequestID":"checkout-failure","ResultCode":1032,"ResultDesc":"cancelled by user"}}}`
	first := serveMPesaCallback(t, handler, database, registry, site, body)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"payment_status":"Failed"`) {
		t.Fatalf("failed callback response=%d body=%s", first.Code, first.Body.String())
	}
	var operationStatus, operationMessage, paymentStatus string
	if err := database.QueryRow("SELECT status, error_message FROM `tabExternal Operation` WHERE name = ?", "OP-FAILURE").Scan(&operationStatus, &operationMessage); err != nil {
		t.Fatal("read failed operation:", err)
	}
	if err := database.QueryRow("SELECT status FROM `tabPayment` WHERE name = ?", "PAY-FAILURE").Scan(&paymentStatus); err != nil {
		t.Fatal("read failed payment:", err)
	}
	if operationStatus != "Failed" || operationMessage != "cancelled by user" || paymentStatus != "Failed" {
		t.Fatalf("failed callback state operation=(%s,%q) payment=%s", operationStatus, operationMessage, paymentStatus)
	}
	retry := serveMPesaCallback(t, handler, database, registry, site, body)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"status":"duplicate"`) {
		t.Fatalf("same terminal failure retry response=%d body=%s", retry.Code, retry.Body.String())
	}
	var processedEvents, duplicateEvents int
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = 'Callback' AND processing_status = 'Processed'", "OP-FAILURE").Scan(&processedEvents); err != nil {
		t.Fatal("count processed callback events:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM `tabExternal Operation Event` WHERE operation = ? AND event_type = 'Callback' AND processing_status = 'Duplicate'", "OP-FAILURE").Scan(&duplicateEvents); err != nil {
		t.Fatal("count duplicate callback events:", err)
	}
	if processedEvents != 1 || duplicateEvents != 1 {
		t.Fatalf("failure callback events processed=%d duplicate=%d; want 1/1", processedEvents, duplicateEvents)
	}
}

func serveMPesaCallback(t *testing.T, handler *Handler, db *sql.DB, reg *doctype.Registry, site, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/mpesa/stk/test-callback-token", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	configureMPesaCallbackTestToken(t, db, site, kdb.Resolve("mysql"))
	bindMPesaCallbackTestIdentity(ctx, site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	handler.HandleMPesaSTKCallback(ctx)
	return recorder
}

func configureMPesaCallbackTestToken(t *testing.T, db *sql.DB, site string, dialect kdb.Dialect) {
	t.Helper()
	if err := secret.NewStore(db, dialect).Set(site, "mpesa_callback_token", "test-callback-token"); err != nil {
		t.Fatalf("configure M-Pesa callback token: %v", err)
	}
	return
}

func bindMPesaCallbackTestIdentity(c *gin.Context, site string) {
	c.Set("site_id", site)
	c.Params = gin.Params{{Key: "token", Value: "test-callback-token"}}
}

func registerPOSIntegrationView(reg *doctype.Registry) {
	reg.Views.Register(&doctype.View{
		Name: "POS integration", Route: "/pos", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "complete", Trigger: "on_click", Type: "create_transaction",
			Config: map[string]any{
				"target_doctype": "Sale", "reference_field": "reference", "reference_prefix": "POS",
				"related_records": []any{map[string]any{"key": "payment", "doctype": "Payment", "fields": map[string]any{
					"reference": "PAY-$records.sale.name", "sale": "$records.sale.name", "amount": "$context.total", "method": "$context.payment_method", "assignee": "$actor",
				}}},
				"child_table": "Sale Item", "line_source": "cart",
				"line_fields":   map[string]any{"product": "product", "unit_price": "rate", "quantity": "quantity"},
				"line_defaults": map[string]any{"quantity": 1},
				"totals": map[string]any{
					"subtotal_field": "subtotal", "total_field": "total",
					"lines": map[string]any{"quantity_field": "quantity", "unit_price_field": "unit_price", "line_total_field": "line_total"},
				},
			},
		}}}},
	})
}

func registerPOSIntegrationTypes(t *testing.T, db *sql.DB, site string, reg *doctype.Registry, dialects ...kdb.Dialect) {
	t.Helper()
	dialect := kdb.Resolve("mysql")
	if len(dialects) > 0 && dialects[0] != nil {
		dialect = dialects[0]
	}
	reg.Register(&doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Data"},
		{Fieldname: "quantity", Fieldtype: "Float"},
		{Fieldname: "unit_price", Fieldtype: "Currency"},
		{Fieldname: "line_total", Fieldtype: "Currency"},
	}})
	reg.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "reference", Fieldtype: "Data", Reqd: true},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
		{Fieldname: "subtotal", Fieldtype: "Currency"},
		{Fieldname: "total", Fieldtype: "Currency"},
		{Fieldname: "payment_method", Fieldtype: "Data"},
	}})
	reg.Register(&doctype.DocType{Name: "Payment", Fields: []doctype.Field{
		{Fieldname: "reference", Fieldtype: "Data", Reqd: true},
		{Fieldname: "sale", Fieldtype: "Link", Options: "Sale"},
		{Fieldname: "amount", Fieldtype: "Currency"},
		{Fieldname: "method", Fieldtype: "Data"},
		{Fieldname: "status", Fieldtype: "Data", Default: "Draft"},
		{Fieldname: "assignee", Fieldtype: "Data"},
		{Fieldname: "external_operation", Fieldtype: "Link", Options: "External Operation"},
		{Fieldname: "phone_number", Fieldtype: "Data"},
		{Fieldname: "provider_reference", Fieldtype: "Data"},
	}})
	reg.Register(&doctype.DocType{Name: "External Operation", Fields: []doctype.Field{
		{Fieldname: "operation_type", Fieldtype: "Data", Reqd: true},
		{Fieldname: "purpose", Fieldtype: "Data", Reqd: true},
		{Fieldname: "source_doctype", Fieldtype: "Data", Reqd: true},
		{Fieldname: "source_name", Fieldtype: "Data"},
		{Fieldname: "provider", Fieldtype: "Data", Reqd: true},
		{Fieldname: "status", Fieldtype: "Data", Reqd: true},
		{Fieldname: "amount", Fieldtype: "Currency"},
		{Fieldname: "currency", Fieldtype: "Data"},
		{Fieldname: "contact_reference", Fieldtype: "Data"},
		{Fieldname: "provider_reference", Fieldtype: "Data"},
		{Fieldname: "provider_request_id", Fieldtype: "Data"},
		{Fieldname: "idempotency_key", Fieldtype: "Data", Unique: true},
		{Fieldname: "request_payload", Fieldtype: "JSON"},
		{Fieldname: "response_payload", Fieldtype: "JSON"},
		{Fieldname: "callback_payload", Fieldtype: "JSON"},
		{Fieldname: "error_message", Fieldtype: "Text"},
		{Fieldname: "initiated_by", Fieldtype: "Data"},
		{Fieldname: "initiated_at", Fieldtype: "Datetime", Default: "now"},
		{Fieldname: "completed_at", Fieldtype: "Datetime"},
		{Fieldname: "last_status_at", Fieldtype: "Datetime"},
	}})
	reg.Register(&doctype.DocType{Name: "External Operation Event", Fields: []doctype.Field{
		{Fieldname: "operation", Fieldtype: "Link", Options: "External Operation", Reqd: true},
		{Fieldname: "direction", Fieldtype: "Data"},
		{Fieldname: "event_type", Fieldtype: "Data"},
		{Fieldname: "provider", Fieldtype: "Data"},
		{Fieldname: "provider_reference", Fieldtype: "Data"},
		{Fieldname: "previous_status", Fieldtype: "Data"},
		{Fieldname: "new_status", Fieldtype: "Data"},
		{Fieldname: "request_payload", Fieldtype: "JSON"},
		{Fieldname: "response_payload", Fieldtype: "JSON"},
		{Fieldname: "processing_status", Fieldtype: "Data"},
		{Fieldname: "error_message", Fieldtype: "Text"},
		{Fieldname: "idempotency_key", Fieldtype: "Data", Unique: true},
		{Fieldname: "received_at", Fieldtype: "Datetime", Default: "now"},
		{Fieldname: "processed_at", Fieldtype: "Datetime", Default: "now"},
	}})
	for _, name := range []string{"Sale", "Payment", "External Operation", "External Operation Event"} {
		reg.Permissions.SetPermission(&doctype.Permission{Doctype: name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true, Submit: true})
	}
	if err := schema.MigrateSiteFromRegistry(db, site, reg, dialect); err != nil {
		t.Fatalf("migrate POS integration schema: %v", err)
	}
}

func TestPublicCreateRouteUsesKernelAndKeepsPublicFieldProjection(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	dt := reg.Get("TestDoc")
	dt.PublicAccess = &doctype.PublicAccess{Enabled: true, Fields: []string{"title"}}
	reg.Register(dt)
	reg.Views.Register(&doctype.View{
		Name: "Public request", Route: "/request", SourceDocType: "TestDoc",
		PublicAccess: &doctype.ViewPublicAccess{Enabled: true, AllowMutations: true},
	})
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/v?route=/request", strings.NewReader(`{"title":"Public request","status":"private","unlisted":"discard"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	handler.HandlePublicCreate(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("public create status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode public form response: %v", err)
	}
	data, ok := response.Data.(map[string]any)
	if !ok || data["title"] != "Public request" || data["name"] == nil || data["status"] != nil || data["unlisted"] != nil {
		t.Fatalf("public response leaked fields or lost created record: %#v", response.Data)
	}
	name := data["name"].(string)
	var title, status sql.NullString
	if err := db.QueryRow("SELECT title, status FROM `tabTestDoc` WHERE name = ?", name).Scan(&title, &status); err != nil {
		t.Fatal("read submitted record:", err)
	}
	if title.String != "Public request" || status.Valid {
		t.Fatalf("public mutation persisted title=%q status=%#v", title.String, status)
	}
}

type resourceHTTPMethod string

const (
	httpMethodPost   resourceHTTPMethod = "POST"
	httpMethodPut    resourceHTTPMethod = "PUT"
	httpMethodDelete resourceHTTPMethod = "DELETE"
)

func serveResourceMutation(t *testing.T, handler *Handler, db *sql.DB, reg *doctype.Registry, site string, method resourceHTTPMethod, path, title, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	var body string
	if method != httpMethodDelete {
		encoded, err := json.Marshal(map[string]string{"title": title})
		if err != nil {
			t.Fatal(err)
		}
		body = string(encoded)
	}
	return serveResourceMutationBody(t, handler, db, reg, site, method, path, body, idempotencyKey, []string{doctype.AdminRole})
}

func serveResourceMutationBody(t testing.TB, handler *Handler, db *sql.DB, reg *doctype.Registry, site string, method resourceHTTPMethod, path, body, idempotencyKey string, roles []string) *httptest.ResponseRecorder {
	return serveResourceMutationBodyWithDBType(t, handler, db, reg, site, method, path, body, idempotencyKey, roles, "mysql")
}

func serveResourceMutationBodyWithDBType(t testing.TB, handler *Handler, db *sql.DB, reg *doctype.Registry, site string, method resourceHTTPMethod, path, body, idempotencyKey string, roles []string, dbType string) *httptest.ResponseRecorder {
	execution := integrationTestExecution(t, site, "admin@example.test", roles)
	return serveResourceMutationWithExecution(t, handler, db, reg, site, method, path, body, idempotencyKey, dbType, execution)
}

func serveResourceMutationWithExecution(t testing.TB, handler *Handler, db *sql.DB, reg *doctype.Registry, site string, method resourceHTTPMethod, path, body, idempotencyKey, dbType string, execution policy.Execution) *httptest.ResponseRecorder {
	t.Helper()
	if !execution.Valid() || execution.Actor().Site != site {
		t.Fatalf("resource mutation test execution is missing or bound to a different route: tenant=%q site=%q", execution.Tenant(), execution.Actor().Site)
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(string(method), path, strings.NewReader(body))
	if idempotencyKey != "" {
		ctx.Request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if method != httpMethodDelete {
		ctx.Request.Header.Set("Content-Type", "application/json")
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	doctypeName, err := url.PathUnescape(parts[2])
	if err != nil {
		t.Fatalf("unescape DocType route segment: %v", err)
	}
	ctx.Params = gin.Params{{Key: "doctype", Value: doctypeName}}
	if len(parts) > 3 {
		name, err := url.PathUnescape(parts[3])
		if err != nil {
			t.Fatalf("unescape record route segment: %v", err)
		}
		ctx.Params = append(ctx.Params, gin.Param{Key: "name", Value: name})
	}
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", dbType)
	actor := execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	ctx.Set("user", user)
	primaryRole := ""
	if len(actor.Roles) > 0 {
		primaryRole = actor.Roles[0]
	}
	ctx.Set("user_role", primaryRole)
	ctx.Set("user_roles", append([]string(nil), actor.Roles...))
	ctx.Set("policy_execution", execution)
	switch method {
	case httpMethodPost:
		handler.HandleCreate(ctx)
	case httpMethodPut:
		handler.HandleUpdate(ctx)
	case httpMethodDelete:
		handler.HandleDelete(ctx)
	}
	return recorder
}

func serveManifestAction(t *testing.T, handler *Handler, site string, db *sql.DB, reg *doctype.Registry, actionID string, actionContext map[string]any, databaseTypes ...string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	viewName, componentID := "", ""
	for _, view := range reg.Views.All() {
		for _, component := range view.Components {
			for _, action := range component.Actions {
				if action.ID == actionID {
					viewName, componentID = view.Name, component.ID
					break
				}
			}
		}
	}
	if viewName == "" || componentID == "" {
		t.Fatalf("test action %q is not registered on a view component", actionID)
	}
	body, err := json.Marshal(map[string]any{"view": viewName, "component": componentID, "context": actionContext})
	if err != nil {
		t.Fatal("encode manifest action request:", err)
	}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/view/action/"+actionID, strings.NewReader(string(body)))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "actionId", Value: actionID}}
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	databaseType := "mysql"
	if len(databaseTypes) > 0 && databaseTypes[0] != "" {
		databaseType = databaseTypes[0]
	}
	ctx.Set("site_db_type", databaseType)
	ctx.Set("user", "admin@example.test")
	ctx.Set("user_role", doctype.AdminRole)
	ctx.Set("user_roles", []string{doctype.AdminRole})
	execution, err := policy.NewVerifiedExecution(contract.TenantID(site), contract.ActorContext{
		PrincipalID: "admin@example.test", PrincipalType: contract.PrincipalHuman,
		SubjectUserID: "admin@example.test", Site: site, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create verified manifest-action test execution: %v", err)
	}
	ctx.Set("policy_execution", execution)
	handler.HandleViewAction(ctx)
	return recorder
}

func requireRESTDocument(t testing.TB, body []byte, wantDocType string) map[string]any {
	t.Helper()
	var response struct {
		Data map[string]any `json:"data"`
		Meta *Meta          `json:"meta"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode REST response: %v body=%s", err, body)
	}
	if response.Meta == nil || response.Meta.DocType != wantDocType {
		t.Fatalf("REST meta = %#v, want doctype %q", response.Meta, wantDocType)
	}
	return response.Data
}

func requireActionDocument(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode action response: %v body=%s", err, body)
	}
	if response.Data == nil {
		t.Fatalf("action response has no data document: %s", body)
	}
	return response.Data
}

func resourceIntegrationRegistry() *doctype.Registry {
	dt := &doctype.DocType{Name: "TestDoc", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data", Label: "Title", Reqd: true},
		{Fieldname: "serial", Fieldtype: "Data", Label: "Serial", Unique: true},
		{Fieldname: "status", Fieldtype: "Data", Label: "Status"},
	}}
	sale := &doctype.DocType{Name: "Sales Invoice", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data", Label: "Title", Reqd: true},
		{Fieldname: "total_amount", Fieldtype: "Currency", Label: "Total", Reqd: true},
	}}
	payment := &doctype.DocType{Name: "Payment", Fields: []doctype.Field{
		{Fieldname: "sales_invoice", Fieldtype: "Link", Options: "Sales Invoice"},
		{Fieldname: "amount", Fieldtype: "Currency", Reqd: true},
		{Fieldname: "method", Fieldtype: "Data", Reqd: true},
	}}
	reg := doctype.NewRegistry()
	docTypes := []*doctype.DocType{dt, sale, payment}
	permissions := make([]*doctype.Permission, 0, len(docTypes))
	for _, recordType := range docTypes {
		permissions = append(permissions, &doctype.Permission{
			Doctype: recordType.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true, Submit: true,
		})
	}
	reg.LoadFull(docTypes, []*doctype.Role{{Name: doctype.AdminRole}}, permissions)
	return reg
}

func newResourceIntegrationHandler(t testing.TB, measuredDriver ...string) (*Handler, *sql.DB, *doctype.Registry, string) {
	t.Helper()
	database, site := newResourceIntegrationDB(t, measuredDriver...)
	dialect := kdb.Resolve("mysql")
	reg := resourceIntegrationRegistry()
	for _, ddl := range dialect.SystemTableSQL() {
		if !strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
			continue
		}
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create system tables: %v", err)
		}
	}
	for _, ddl := range kdb.OutboxTablesMySQL() {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create outbox tables: %v", err)
		}
	}
	for _, ddl := range kdb.KernelTablesMySQL() {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create kernel tables: %v", err)
		}
	}
	for _, ddl := range configstore.ConfigRevisionSchema(dialect) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create tenant config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(t.Context(), database, dialect); err != nil {
		t.Fatalf("seed tenant config revision: %v", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
		t.Fatalf("migrate site schema: %v", err)
	}
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	t.Cleanup(func() { _ = database.Close() })
	return handler, database, reg, site
}

func integrationTestExecution(t testing.TB, site, user string, roles []string) policy.Execution {
	t.Helper()
	execution, err := policy.NewVerifiedExecution(contract.TenantID(site), contract.ActorContext{
		PrincipalID: user, PrincipalType: contract.PrincipalHuman,
		SubjectUserID: user, Site: site, Roles: roles, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create verified integration execution: %v", err)
	}
	return execution
}

func newResourceIntegrationDB(t testing.TB, measuredDriver ...string) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	if err := root.Ping(); err != nil {
		root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_api_kernel_%d", time.Now().UnixNano()%1_000_000_000)
	quoted := "`" + name + "`"
	if _, err := root.Exec("CREATE DATABASE " + quoted + " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		root.Close()
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + quoted)
		_ = root.Close()
	})
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse mysql dsn: %v", err)
	}
	cfg.DBName = name
	cfg.Params = map[string]string{"parseTime": "true", "charset": "utf8mb4"}
	driverName := "mysql"
	if len(measuredDriver) > 0 && measuredDriver[0] != "" {
		driverName = measuredDriver[0]
	}
	database, err := sql.Open(driverName, cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open isolated db: %v", err)
	}
	if err := database.Ping(); err != nil {
		database.Close()
		t.Fatalf("ping isolated db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, name
}
