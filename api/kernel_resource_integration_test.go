//go:build integration

package api

import (
	"context"
	"database/sql"
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

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/script"
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

	updated := serveResourceMutation(t, handler, db, reg, site, httpMethodPut, "/api/resource/TestDoc/"+name, "Updated by MVP", "")
	if updated.Code != 200 {
		t.Fatalf("update status = %d, want 200: %s", updated.Code, updated.Body.String())
	}
	updatedDoc := requireRESTDocument(t, updated.Body.Bytes(), "TestDoc")
	if updatedDoc["name"] != name || updatedDoc["title"] != "Updated by MVP" {
		t.Fatalf("update response contract mismatch: %#v", updatedDoc)
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
	if rows != 0 || audits != 3 || events != 3 {
		t.Fatalf("CRUD side effects = rows:%d audits:%d outbox:%d, want rows:0 audits:3 outbox:3 (changed-payload retry must not write)", rows, audits, events)
	}
	var receipts int
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", site, "create-key-1").Scan(&receipts); err != nil {
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
	if owner != "owner@example.test" || modifiedBy != "script@example.test" {
		t.Fatalf("script create attribution owner=%q modified_by=%q", owner, modifiedBy)
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
		t.Fatalf("script CRUD side effects = rows:%d audits:%d outbox:%d; want rows:0 audits:3 outbox:3", rows, audits, events)
	}
	var canonicalAudits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM _kora_operation_audit
		WHERE site = ? AND source = 'integration' AND actor_user = 'script@example.test'
		AND principal_id = 'script@example.test'
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
	store := &script.Store{DB: db, Dialect: kdb.Resolve("mysql")}
	runner := &nestedScriptMutationRunner{}
	tx := *handler.TxManager
	tx.CurrentUser = "script@example.test"
	tx.CurrentUserRole = doctype.AdminRole
	tx.CurrentUserRoles = []string{doctype.AdminRole}
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

func TestWorkflowActionRouteUsesKernelAndPreservesRESTContract(t *testing.T) {
	handler, db, reg, site := newResourceIntegrationHandler(t)
	reg.Workflows.Register(&doctype.Workflow{
		Name: "TestDoc Lifecycle", DocumentType: "TestDoc", IsActive: true, WorkflowStateField: "status",
		States:      []doctype.WorkflowState{{State: "Draft", DocStatus: 0}, {State: "Done", DocStatus: 1}},
		Transitions: []doctype.WorkflowTransition{{Action: "Complete", From: "Draft", To: "Done", Allowed: doctype.AdminRole}},
	})
	created := serveResourceMutation(t, handler, db, reg, site, httpMethodPost, "/api/resource/TestDoc", "Workflow record", "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	createdDoc := requireRESTDocument(t, created.Body.Bytes(), "TestDoc")
	name := createdDoc["name"].(string)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/resource/TestDoc/"+name+"/workflow_action", strings.NewReader(`{"action":"Complete"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}, {Key: "name", Value: name}}
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	ctx.Set("user", "admin@example.test")
	ctx.Set("user_role", doctype.AdminRole)
	ctx.Set("user_roles", []string{doctype.AdminRole})
	handler.HandleWorkflowAction(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("workflow action status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var response Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode workflow response: %v", err)
	}
	if response.Meta == nil || response.Meta.DocType != "TestDoc" {
		t.Fatalf("workflow response metadata = %#v", response.Meta)
	}
	data, ok := response.Data.(map[string]any)
	if !ok || data["name"] != name || data["status"] != "Done" || data["doc_status"] != float64(1) {
		t.Fatalf("workflow response data = %#v", response.Data)
	}
	var state string
	var auditCount, outboxCount int
	if err := db.QueryRow("SELECT status FROM `tabTestDoc` WHERE name = ?", name).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE doc_name = ? AND command_name = ?", name, "record.workflow_transition").Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, name).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if state != "Done" || auditCount != 1 || outboxCount != 2 {
		t.Fatalf("workflow mutation state=%q audit=%d outbox=%d; want Done/1/2", state, auditCount, outboxCount)
	}
}

func TestWorkflowActionRejectionsDoNotWriteOrEmitEffects(t *testing.T) {
	handler, database, registry, site := newResourceIntegrationHandler(t)
	registry.Workflows.Register(&doctype.Workflow{
		Name: "TestDoc Lifecycle", DocumentType: "TestDoc", IsActive: true, WorkflowStateField: "status",
		States:      []doctype.WorkflowState{{State: "Draft", DocStatus: 0}, {State: "Done", DocStatus: 1}},
		Transitions: []doctype.WorkflowTransition{{Action: "Complete", From: "Draft", To: "Done", Allowed: doctype.AdminRole}},
	})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "TestDoc", Role: "Reader", Read: true, Write: true})
	created := serveResourceMutation(t, handler, database, registry, site, httpMethodPost, "/api/resource/TestDoc", "Workflow rejection", "")
	if created.Code != http.StatusCreated {
		t.Fatalf("seed create status = %d: %s", created.Code, created.Body.String())
	}
	createdDoc := requireRESTDocument(t, created.Body.Bytes(), "TestDoc")
	name := createdDoc["name"].(string)

	invoke := func(action string, roles []string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/resource/TestDoc/"+url.PathEscape(name)+"/workflow_action", strings.NewReader(`{"action":"`+action+`"}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}, {Key: "name", Value: name}}
		ctx.Set("site_name", site)
		ctx.Set("site_db", database)
		ctx.Set("site_registry", registry)
		ctx.Set("site_db_type", "mysql")
		ctx.Set("user", "workflow@example.test")
		if len(roles) > 0 {
			ctx.Set("user_role", roles[0])
		}
		ctx.Set("user_roles", roles)
		handler.HandleWorkflowAction(ctx)
		return recorder
	}

	for _, test := range []struct {
		name       string
		action     string
		roles      []string
		wantStatus int
	}{
		{name: "unavailable transition", action: "Archive", roles: []string{doctype.AdminRole}, wantStatus: http.StatusBadRequest},
		{name: "submit denied", action: "Complete", roles: []string{"Reader"}, wantStatus: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := invoke(test.action, test.roles)
			if response.Code != test.wantStatus {
				t.Fatalf("workflow response status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	var state string
	var docStatus, audits, events int
	if err := database.QueryRow("SELECT COALESCE(status, 'Draft'), doc_status FROM `tabTestDoc` WHERE name = ?", name).Scan(&state, &docStatus); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", site, name).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", site, name).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if state != "Draft" || docStatus != 0 || audits != 1 || events != 1 {
		t.Fatalf("rejected workflow attempts changed state=%q doc_status=%d audits=%d outbox=%d; want Draft/0/1/1", state, docStatus, audits, events)
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
		t.Fatalf("manifest action side effects = audit:%d outbox:%d; want 2/2", auditCount, outboxCount)
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
	if err := db.QueryRow("SELECT COUNT(*) FROM `tabPayment` WHERE sale = ? AND method = ? AND amount = ? AND status = ?", saleName, "cash", 250, "Succeeded").Scan(&paymentCount); err != nil {
		t.Fatal("query POS payment:", err)
	}
	if paymentCount != 1 {
		t.Fatalf("POS completed sale payment count = %d, want 1", paymentCount)
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
		"receipt":   "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?",
	} {
		var count int
		args := []any(nil)
		if table == "audit" || table == "outbox" {
			args = []any{site}
		}
		if table == "receipt" {
			args = []any{site, "external-operation:pos-test-initiation-event-failure"}
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
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", site, "external-operation-result:"+failedOperationName).Scan(&outcomeReceipts); err != nil {
		t.Fatalf("count failed outcome receipts: %v", err)
	}
	if failedOperationStatus != "Initiating" || failedPaymentStatus != "Pending" || failedEvents != 0 || completedOperationAudits != 1 || outcomeReceipts != 0 {
		t.Fatalf("failed outcome bundle left operation=%s payment=%s events=%d operation_audits=%d outcome_receipts=%d", failedOperationStatus, failedPaymentStatus, failedEvents, completedOperationAudits, outcomeReceipts)
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
		t.Fatalf("failed provider state operation=(%q,%q) payment=%q failure_events=%d operation_audits=%d payment_audits=%d record_outbox=%d total_outbox=%d; want Failed/Failed with error, 1 event, 2/2 audits and 4/5 outbox", operationStatus, operationError, paymentStatus, eventCount, operationAudits, paymentAudits, operationEvents, totalOutbox)
	}

	if _, err := database.Exec("CREATE TRIGGER reject_operation_failure_event BEFORE INSERT ON `tabExternal Operation Event` FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'simulated event write failure'"); err != nil {
		t.Fatalf("install event failure trigger: %v", err)
	}
	secondRequest := map[string]any{"client_reference": "pos-test-operation-event-failure", "total": 125.0, "payment_method": "mpesa", "customer_phone": "+254700000002"}
	failedPersistence := serveManifestAction(t, handler, site, database, registry, "initiate-failing", secondRequest)
	if failedPersistence.Code != http.StatusInternalServerError {
		t.Fatalf("failure-event persistence response = %d, want 500: %s", failedPersistence.Code, failedPersistence.Body.String())
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
	if eventCount != 0 || operationAudits != 2 || operationEvents != 2 {
		t.Fatalf("event-write rollback left events=%d audits=%d record_outbox=%d; want 0/2/2 (only initial create bundle)", eventCount, operationAudits, operationEvents)
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
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/mpesa/callback", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
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
	gin.SetMode(gin.TestMode)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/mpesa/callback", strings.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set("site_name", site)
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
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/mpesa/callback", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", "mysql")
	handler.HandleMPesaSTKCallback(ctx)
	return recorder
}

func registerPOSIntegrationView(reg *doctype.Registry) {
	reg.Views.Register(&doctype.View{
		Name: "POS integration", Route: "/pos", Type: "register", SourceDocType: "Sale",
		Components: []doctype.ViewComponent{{ID: "payment", Actions: []doctype.ViewAction{{
			ID: "complete", Trigger: "on_click", Type: "create_transaction",
			Config: map[string]any{
				"target_doctype": "Sale", "reference_field": "reference", "reference_prefix": "POS",
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
		{Fieldname: "status", Fieldtype: "Data"},
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
	t.Helper()
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
	ctx.Set("user", "admin@example.test")
	primaryRole := ""
	if len(roles) > 0 {
		primaryRole = roles[0]
	}
	ctx.Set("user_role", primaryRole)
	ctx.Set("user_roles", roles)
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
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
		t.Fatalf("migrate site schema: %v", err)
	}
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	t.Cleanup(func() { _ = database.Close() })
	return handler, database, reg, site
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
