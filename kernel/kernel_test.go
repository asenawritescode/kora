//go:build integration
// +build integration

// First-vertical-slice evidence suite (SPEC-014 scenarios, KERNEL tickets):
// canonical command execution through one kernel path with tenant isolation,
// authorization parity, idempotency, optimistic concurrency, transaction
// rollback atomicity, audit, outbox atomicity, and durable delivery via NATS
// JetStream. PostgreSQL is the reference dialect; this suite runs against
// MySQL as the currently available integration harness (DB-005 records the
// compatibility matrix).
package kernel_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	mysql "github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/workflow"
)

func newSiteDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("MySQL not available: %v", err)
	}
	name := fmt.Sprintf("kora_kernel_%d", time.Now().UnixNano()%1_000_000)
	if _, err := db.Exec(fmt.Sprintf("CREATE DATABASE `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", name)); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() { db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", name)) })

	cfg, _ := mysql.ParseDSN(dsn)
	cfg.DBName = name
	cfg.Params = map[string]string{"parseTime": "true", "charset": "utf8mb4"}
	sdb, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open site db: %v", err)
	}
	return sdb, name
}

type siteFixture struct {
	DB             *sql.DB
	Name           string
	Registry       *doctype.Registry
	Dialect        kdb.Dialect
	StatusPolicies *testStatusPolicyRepository
}

type testStatusPolicyRepository struct {
	mu       sync.RWMutex
	policies map[string]workflow.StatusPolicy
}

func (r *testStatusPolicyRepository) GetStatusPolicyInTx(_ context.Context, _ *sql.Tx, _ contract.TenantID, table string) (*workflow.StatusPolicy, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	statusPolicy, found := r.policies[table]
	if !found {
		return nil, false, nil
	}
	return &statusPolicy, true, nil
}

func (r *testStatusPolicyRepository) Set(statusPolicy workflow.StatusPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.policies == nil {
		r.policies = make(map[string]workflow.StatusPolicy)
	}
	r.policies[statusPolicy.Table] = statusPolicy
}

func newSite(t *testing.T, name string) *siteFixture {
	t.Helper()
	sdb, dbName := newSiteDB(t)
	dialect := kdb.Resolve("mysql")
	for _, ddl := range dialect.SystemTableSQL() {
		if !strings.HasPrefix(strings.TrimSpace(ddl), "CREATE TABLE IF NOT EXISTS") {
			continue
		}
		if _, err := sdb.Exec(ddl); err != nil {
			t.Fatalf("system table: %v", err)
		}
	}
	for _, ddl := range kdb.OutboxTablesMySQL() {
		if _, err := sdb.Exec(ddl); err != nil {
			t.Fatalf("outbox table: %v", err)
		}
	}
	for _, ddl := range kdb.KernelTablesMySQL() {
		if _, err := sdb.Exec(ddl); err != nil {
			t.Fatalf("kernel table: %v", err)
		}
	}

	taskDT := &doctype.DocType{
		Name:   "Task",
		Module: "Kernel Test",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title", Reqd: true},
			{Fieldname: "serial", Fieldtype: "Data", Label: "Serial", Unique: true},
			{Fieldname: "status", Fieldtype: "Select", Label: "Status", Options: "Open\nDone"},
			{Fieldname: "priority", Fieldtype: "Data", Label: "Priority", Default: "Normal"},
			{Fieldname: "related_task", Fieldtype: "Link", Label: "Related Task", Options: "Task"},
		},
	}
	roles := []*doctype.Role{{Name: "Administrator"}, {Name: "Creator"}, {Name: "OwnerEditor"}}
	roles = append(roles, &doctype.Role{Name: "Writer"})
	perms := []*doctype.Permission{
		{Doctype: "Task", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true, Submit: true},
		{Doctype: "Task", Role: "Creator", Read: true, Create: true}, // create-only: no write
		{Doctype: "Task", Role: "OwnerEditor", Read: true, Write: true, Create: true, IfOwner: true},
		{Doctype: "Task", Role: "Writer", Read: true, Write: true, Create: true},
	}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{taskDT}, roles, perms)
	if err := schema.MigrateSiteFromRegistry(sdb, dbName, reg, dialect); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &siteFixture{DB: sdb, Name: name, Registry: reg, Dialect: dialect, StatusPolicies: &testStatusPolicyRepository{}}
}

func newKernel(s *siteFixture) *kernel.Kernel {
	engine := kernel.New(s.Dialect, outbox.NewSQLWriter(s.Dialect))
	engine.WorkflowStatusPolicies = s.StatusPolicies
	return engine
}

type configuredCommandHookRunner struct {
	db     *sql.DB
	events []script.Event
}

func (r *configuredCommandHookRunner) Execute(_ context.Context, req script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.events = append(r.events, req.Event)
	if req.Event == script.EventBeforeInsert {
		document := make(map[string]any, len(req.Document))
		for key, value := range req.Document {
			document[key] = value
		}
		document["title"] = fmt.Sprint(document["title"]) + "-before-hook"
		return &script.ExecuteResult{Document: document, Modified: true}, nil
	}
	if req.Event == script.EventBeforeSave {
		document := make(map[string]any, len(req.Document))
		for key, value := range req.Document {
			document[key] = value
		}
		document["title"] = fmt.Sprint(document["title"]) + "-before-save-hook"
		return &script.ExecuteResult{Document: document, Modified: true}, nil
	}
	if req.Event == script.EventAfterInsert {
		var count int
		if err := r.db.QueryRow("SELECT COUNT(*) FROM `tabTask` WHERE name = ?", req.Document["name"]).Scan(&count); err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, fmt.Errorf("after_insert ran before commit; visible row count = %d", count)
		}
	}
	if req.Event == script.EventAfterSave {
		var title string
		if err := r.db.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", req.Document["name"]).Scan(&title); err != nil {
			return nil, err
		}
		if title != req.Document["title"] {
			return nil, fmt.Errorf("after_save observed %q; committed title is %q", req.Document["title"], title)
		}
	}
	return &script.ExecuteResult{}, nil
}
func (*configuredCommandHookRunner) Validate(string) error { return nil }
func (*configuredCommandHookRunner) Close() error          { return nil }

type scopedComputedFieldRunner struct{}

func (scopedComputedFieldRunner) Execute(_ context.Context, req script.ExecuteRequest) (*script.ExecuteResult, error) {
	if req.Event != script.EventComputed || req.ScriptName != "site-name" {
		return nil, fmt.Errorf("unexpected computed script request: event=%q name=%q", req.Event, req.ScriptName)
	}
	return &script.ExecuteResult{Result: req.Site}, nil
}
func (scopedComputedFieldRunner) Validate(string) error { return nil }
func (scopedComputedFieldRunner) Close() error          { return nil }

func TestRecordCreateUsesOperationScopedComputedScriptHook(t *testing.T) {
	s := newSite(t, "computed-script-site")
	defer s.DB.Close()
	for _, ddl := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := s.DB.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	dt := &doctype.DocType{Name: "ComputedTask", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data", Reqd: true},
		{Fieldname: "site_value", Fieldtype: "Data", Computed: "@script:site-name"},
	}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
		Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true,
	}})
	if err := schema.MigrateSiteFromRegistry(s.DB, s.Name, registry, s.Dialect); err != nil {
		t.Fatalf("migrate computed DocType: %v", err)
	}
	store := &script.Store{DB: s.DB, Dialect: s.Dialect}
	if err := store.Insert(script.ScriptRecord{
		Name: "site-name", Site: s.Name, ScriptType: script.TypeDocEvent,
		DocType: dt.Name, Event: script.EventComputed, IsActive: true, Script: "return site",
	}); err != nil {
		t.Fatalf("insert computed script: %v", err)
	}
	k := newKernel(s)
	k.TxManager = &orm.TxManager{ScriptRunner: scopedComputedFieldRunner{}, ScriptStore: store}
	payload, _ := json.Marshal(map[string]any{"doctype": dt.Name, "data": map[string]any{"title": "computed"}})
	result, err := k.Execute(context.Background(), s.DB, registry, kernel.Operation{
		Command: kernel.CommandRecordCreate, Payload: payload, Context: opCtx(s.Name, doctype.AdminRole),
	})
	if err != nil {
		t.Fatalf("execute record.create: %v", err)
	}
	mustComplete(t, result)
	var data kernel.ResultData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("decode create result: %v", err)
	}
	var siteValue string
	query := "SELECT site_value FROM " + s.Dialect.QuoteIdent(dt.RawTableName()) + " WHERE name = ?"
	if err := s.DB.QueryRow(query, data.Name).Scan(&siteValue); err != nil {
		t.Fatalf("read computed value: %v", err)
	}
	if siteValue != s.Name {
		t.Fatalf("computed script value = %q, want operation site %q", siteValue, s.Name)
	}
}

func TestConfiguredCommandUsesRecordLifecyclePipeline(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	for _, ddl := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := s.DB.Exec(ddl); err != nil {
			t.Fatalf("create extensibility tables: %v", err)
		}
	}
	store := &script.Store{DB: s.DB, Dialect: s.Dialect}
	for _, record := range []script.ScriptRecord{
		{Name: "task-before-insert", Site: s.Name, ScriptType: script.TypeDocEvent, DocType: "Task", Event: script.EventBeforeInsert, IsActive: true, Script: "return document"},
		{Name: "task-after-insert", Site: s.Name, ScriptType: script.TypeDocEvent, DocType: "Task", Event: script.EventAfterInsert, IsActive: true, Script: "return document"},
		{Name: "task-before-save", Site: s.Name, ScriptType: script.TypeDocEvent, DocType: "Task", Event: script.EventBeforeSave, IsActive: true, Script: "return document"},
		{Name: "task-after-save", Site: s.Name, ScriptType: script.TypeDocEvent, DocType: "Task", Event: script.EventAfterSave, IsActive: true, Script: "return document"},
	} {
		if err := store.Insert(record); err != nil {
			t.Fatalf("insert lifecycle script: %v", err)
		}
	}
	runner := &configuredCommandHookRunner{db: s.DB}
	k := newKernel(s)
	k.TxManager = &orm.TxManager{ScriptRunner: runner, ScriptStore: store}
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, `
name: task.create
namespace: test
version: 1
input:
  record: Task
transaction:
  - create:
      record: Task
      values:
        title: $input.title
`)
	payload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": "from-command"}})
	result := exec(t, s, k, kernel.Operation{Command: "test.task.create", Payload: payload, Context: opCtx(s.Name, "Administrator")})
	mustComplete(t, result)
	var storedTitle string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE title LIKE 'from-command%' ORDER BY creation DESC LIMIT 1").Scan(&storedTitle); err != nil {
		t.Fatal("load created task:", err)
	}
	if storedTitle != "from-command-before-hook-before-save-hook" {
		t.Fatalf("before_insert result = %q, want hook-modified title", storedTitle)
	}
	var storedPriority string
	if err := s.DB.QueryRow("SELECT priority FROM `tabTask` WHERE title = ?", storedTitle).Scan(&storedPriority); err != nil {
		t.Fatal("load configured-command default:", err)
	}
	if storedPriority != "Normal" {
		t.Fatalf("configured-command default priority = %q, want Normal", storedPriority)
	}
	wantCreateEvents := []script.Event{script.EventBeforeInsert, script.EventBeforeSave, script.EventAfterInsert, script.EventAfterSave}
	if len(runner.events) != len(wantCreateEvents) {
		t.Fatalf("create lifecycle events = %#v, want %#v", runner.events, wantCreateEvents)
	}
	for index, event := range wantCreateEvents {
		if runner.events[index] != event {
			t.Fatalf("create lifecycle events = %#v, want %#v", runner.events, wantCreateEvents)
		}
	}
	var createdName string
	if err := s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title='from-command-before-hook-before-save-hook'").Scan(&createdName); err != nil {
		t.Fatal("load created task name:", err)
	}
	runner.events = nil
	mustRegister(t, k, `
name: task.update
namespace: test
version: 1
input:
  record: Task
transaction:
  - update:
      record: Task
      name: $input.name
      values:
        title: $input.title
`)
	updatePayload, _ := json.Marshal(map[string]any{"data": map[string]any{"name": createdName, "title": "edited"}})
	updated := exec(t, s, k, kernel.Operation{Command: "test.task.update", Payload: updatePayload, Context: opCtx(s.Name, "Administrator")})
	mustComplete(t, updated)
	if len(runner.events) != 2 || runner.events[0] != script.EventBeforeSave || runner.events[1] != script.EventAfterSave {
		t.Fatalf("update lifecycle events = %#v, want before_save then after_save", runner.events)
	}
	var updatedTitle string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", createdName).Scan(&updatedTitle); err != nil {
		t.Fatal("load updated task:", err)
	}
	if updatedTitle != "edited-before-save-hook" {
		t.Fatalf("before_save result = %q, want hook-modified title", updatedTitle)
	}
	runner.events = nil
	foreignCreate := createOp(map[string]any{"title": "foreign owner"})
	foreignCreate.Context = opCtx(s.Name, "Administrator")
	foreignCreate.Context.User = "another-owner"
	foreignCreate.Context.Owner = "another-owner"
	foreignCreated, err := k.Execute(context.Background(), s.DB, s.Registry, foreignCreate)
	if err != nil || foreignCreated.Error != nil {
		t.Fatalf("seed owner-scoped record: result=%+v err=%v", foreignCreated.Error, err)
	}
	var foreignRecord kernel.ResultData
	if err := json.Unmarshal(foreignCreated.Data, &foreignRecord); err != nil {
		t.Fatal("decode owner-scoped record:", err)
	}
	foreignUpdatePayload, _ := json.Marshal(map[string]any{"data": map[string]any{"name": foreignRecord.Name, "title": "unauthorized edit"}})
	foreignUpdate := kernel.Operation{Command: "test.task.update", Payload: foreignUpdatePayload, Context: opCtx(s.Name, "OwnerEditor")}
	unauthorized := exec(t, s, k, foreignUpdate)
	if unauthorized.Error == nil || unauthorized.Error.Type != contract.CodeNotFound {
		t.Fatalf("owner-scoped command update error = %+v, want not found", unauthorized.Error)
	}
	var unchangedTitle string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", foreignRecord.Name).Scan(&unchangedTitle); err != nil {
		t.Fatal("load owner-scoped record after denied update:", err)
	}
	if unchangedTitle != "foreign owner-before-hook-before-save-hook" {
		t.Fatalf("owner-scoped record changed to %q after denied command", unchangedTitle)
	}
	runner.events = nil
	mustRegister(t, k, `
name: task.create_then_fail
namespace: test
version: 1
input:
  record: Task
transaction:
  - create:
      record: Task
      values:
        title: $input.title
  - update:
      record: Task
      name: $input.missing_record
      values:
        status: Done
`)
	failedPayload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": "must-rollback"}})
	failed := exec(t, s, k, kernel.Operation{Command: "test.task.create_then_fail", Payload: failedPayload, Context: opCtx(s.Name, "Administrator")})
	if failed.Error == nil {
		t.Fatal("later failing step unexpectedly committed")
	}
	if got := s.count(t, "SELECT COUNT(*) FROM `tabTask` WHERE title='must-rollback-before-hook'"); got != 0 {
		t.Fatalf("failed config command left %d created records", got)
	}
	wantFailedEvents := []script.Event{script.EventBeforeInsert, script.EventBeforeSave}
	if len(runner.events) != len(wantFailedEvents) {
		t.Fatalf("failed command lifecycle events = %#v; after hooks must not run before commit", runner.events)
	}
	for index, event := range wantFailedEvents {
		if runner.events[index] != event {
			t.Fatalf("failed command lifecycle events = %#v; after hooks must not run before commit", runner.events)
		}
	}
}

type opOpt func(*kernel.OperationContext)

func withUser(u string) opOpt            { return func(c *kernel.OperationContext) { c.User = u } }
func withRoles(roles ...string) opOpt    { return func(c *kernel.OperationContext) { c.Roles = roles } }
func withSource(src kernel.Source) opOpt { return func(c *kernel.OperationContext) { c.Source = src } }
func withKey(key string) opOpt           { return func(c *kernel.OperationContext) { c.IdempotencyKey = key } }
func withExpectedVersion(v string) opOpt {
	return func(c *kernel.OperationContext) { c.ExpectedVersion = v }
}
func withExpectedVersionLegacy(v string) opOpt {
	return func(c *kernel.OperationContext) { c.CausationID = "expected:" + v }
}
func withCorrelation(id string) opOpt {
	return func(c *kernel.OperationContext) { c.CorrelationID = id }
}

func createOp(data map[string]any, opts ...opOpt) kernel.Operation {
	return payloadOp("record.create", "", data, opts...)
}

func updateOp(name string, data map[string]any, opts ...opOpt) kernel.Operation {
	return payloadOp("record.update", name, data, opts...)
}

func payloadOp(command, name string, data map[string]any, opts ...opOpt) kernel.Operation {
	payload := map[string]any{"doctype": "Task", "data": data}
	if name != "" {
		payload["name"] = name
	}
	raw, _ := json.Marshal(payload)
	op := kernel.Operation{Command: command, Payload: raw}
	op.Context.Site = "TBD"
	for _, o := range opts {
		o(&op.Context)
	}
	return op
}

func bundleOp(payload kernel.RecordMutationBundlePayload, opts ...opOpt) kernel.Operation {
	raw, _ := json.Marshal(payload)
	op := kernel.Operation{Command: kernel.CommandRecordMutateBundle, Payload: raw}
	op.Context.Site = "TBD"
	for _, option := range opts {
		option(&op.Context)
	}
	return op
}

func exec(t *testing.T, s *siteFixture, k *kernel.Kernel, op kernel.Operation) contract.CommandResult {
	t.Helper()
	op.Context.Site = s.Name
	if op.Context.Actor.PrincipalID == "" {
		user := op.Context.User
		if user == "" {
			user = "Administrator"
		}
		roles := op.Context.Roles
		if len(roles) == 0 && user == "Administrator" {
			roles = []string{"Administrator"}
			op.Context.Roles = roles
		}
		op.Context.Actor = contract.ActorContext{
			PrincipalID:     user,
			PrincipalType:   contract.PrincipalHuman,
			Site:            s.Name,
			Roles:           roles,
			AuthenticatedAt: time.Now(),
		}
	}
	if !op.Context.Execution.Valid() {
		execution, err := policy.NewVerifiedExecution(contract.TenantID(s.Name), op.Context.Actor)
		if err != nil {
			t.Fatalf("create verified kernel-test execution: %v", err)
		}
		op.Context.TenantID = execution.Tenant()
		op.Context.Execution = execution
	}
	res, cerr := k.Execute(context.Background(), s.DB, s.Registry, op)
	t.Logf("op %s → status=%s err=%v replayed=%v", op.Command, res.Status, cerr, res.Replayed)
	return res
}

func mustComplete(t *testing.T, res contract.CommandResult) {
	t.Helper()
	if res.Status != contract.StatusCompleted || res.Error != nil {
		t.Fatalf("expected completed, got %s err=%+v", res.Status, res.Error)
	}
}

func (s *siteFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (s *siteFixture) taskCount(t *testing.T) int {
	return s.count(t, "SELECT COUNT(*) FROM `tabTask`")
}

// TestRecordCreateHappyPath proves the canonical path: one command definition,
// one envelope, mutation + receipt + audit + outbox committed atomically.
func TestRecordCreateHappyPath(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	res := exec(t, s, k, createOp(map[string]any{"title": "First", "status": "Open"}, withKey("idem-1"), withCorrelation("corr-1")))
	mustComplete(t, res)

	var docName string
	if err := s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title = 'First'").Scan(&docName); err != nil {
		t.Fatalf("row not persisted: %v", err)
	}
	if s.taskCount(t) != 1 {
		t.Fatalf("expected exactly 1 task")
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.create'", s.Name); n != 1 {
		t.Fatalf("expected 1 receipt, got %d", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", s.Name, docName); n != 1 {
		t.Fatalf("expected 1 audit row, got %d", n)
	}
	var initialChanges string
	if err := s.DB.QueryRow("SELECT changes_json FROM _kora_record_history WHERE site = ? AND doctype = 'Task' AND doc_name = ?", s.Name, docName).Scan(&initialChanges); err != nil {
		t.Fatalf("record create history not persisted: %v", err)
	}
	if !strings.Contains(initialChanges, `"field":"title"`) || !strings.Contains(initialChanges, `"after":"First"`) {
		t.Fatalf("create history omitted initial visible values: %s", initialChanges)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", s.Name, docName); n != 1 {
		t.Fatalf("expected 1 pending outbox event, got %d", n)
	}

	// KERNEL-009: create path carries after_hash only (64-hex).
	var beforeHash, afterHash string
	if err := s.DB.QueryRow(`SELECT before_hash, after_hash FROM _kora_operation_audit WHERE doc_name = ?`, docName).Scan(&beforeHash, &afterHash); err != nil {
		t.Fatalf("read hashes: %v", err)
	}
	if len(afterHash) != 64 || beforeHash != "" {
		t.Fatalf("create audit must have empty before_hash and 64-char after_hash (got %q/%q)", beforeHash, afterHash)
	}
}

// TestIdempotencyReplayAndKeyReuse covers RFC §7.1 idempotency semantics.
func TestIdempotencyReplayAndKeyReuse(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	first := exec(t, s, k, createOp(map[string]any{"title": "Once"}, withKey("same-key")))
	mustComplete(t, first)

	replay := exec(t, s, k, createOp(map[string]any{"title": "Once"}, withKey("same-key")))
	mustComplete(t, replay)
	if !replay.Replayed {
		t.Fatalf("second identical operation must be flagged Replayed")
	}
	if len(first.Data) == 0 || string(replay.Data) != string(first.Data) {
		t.Fatalf("idempotent replay must return the original result data; first=%s replay=%s", first.Data, replay.Data)
	}
	if s.taskCount(t) != 1 {
		t.Fatalf("replay must not create a second document")
	}

	reuse := exec(t, s, k, createOp(map[string]any{"title": "Different"}, withKey("same-key")))
	if reuse.Error == nil || reuse.Error.Type != contract.CodeIdempotencyKeyReused {
		t.Fatalf("payload mismatch under same key must be IDEMPOTENCY_KEY_REUSED, got %+v", reuse.Error)
	}
	if s.taskCount(t) != 1 {
		t.Fatalf("key reuse must not mutate state")
	}
}

func TestIdempotencyReplaySurvivesTenantRouteRename(t *testing.T) {
	s := newSite(t, "tenant-route-before")
	defer s.DB.Close()
	k := newKernel(s)
	data, _ := json.Marshal(map[string]any{"title": "created once across route rename"})
	payload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: "Task", Data: data})
	const stableTenant = "stable-tenant-route-rename"
	const idempotencyKey = "route-rename-retry-key"
	perform := func(route string) contract.CommandResult {
		t.Helper()
		actor := contract.ActorContext{
			PrincipalID: "route-rename-user", PrincipalType: contract.PrincipalHuman,
			Site: route, Roles: []string{"Administrator"}, AuthenticatedAt: time.Now().UTC(),
		}
		execution, err := policy.NewVerifiedExecution(contract.TenantID(stableTenant), actor)
		if err != nil {
			t.Fatal("create route-specific execution:", err)
		}
		result, commandErr := k.Execute(context.Background(), s.DB, s.Registry, kernel.Operation{
			Command: kernel.CommandRecordCreate, Payload: payload,
			Context: kernel.OperationContext{
				Site: route, TenantID: contract.TenantID(stableTenant), Execution: execution,
				User: actor.PrincipalID, Roles: actor.Roles, IdempotencyKey: idempotencyKey,
			},
		})
		if commandErr != nil {
			t.Fatalf("execute create for route %q: %v", route, commandErr)
		}
		return result
	}
	first := perform("tenant-route-before")
	second := perform("tenant-route-after")
	if first.Status != contract.StatusCompleted || !second.Replayed || second.OperationID != first.OperationID || string(second.Data) != string(first.Data) {
		t.Fatalf("route rename changed receipt: first=%+v second=%+v", first, second)
	}
	if got := s.taskCount(t); got != 1 {
		t.Fatalf("route rename retry created %d records, want one", got)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.create'", stableTenant); got != 1 {
		t.Fatalf("stable-tenant receipt rows = %d, want one", got)
	}
}

func TestIdempotencyReplayRequiresCurrentAuthorization(t *testing.T) {
	s := newSite(t, "permission-revoked-replay")
	defer s.DB.Close()
	k := newKernel(s)
	op := createOp(map[string]any{"title": "authorized once"}, withKey("revoked-permission-key"))
	op.Context = opCtx(s.Name, "Creator")
	first := exec(t, s, k, op)
	mustComplete(t, first)
	if s.taskCount(t) != 1 {
		t.Fatalf("initial command task count = %d, want 1", s.taskCount(t))
	}

	// Simulate the active tenant policy after Create permission has been revoked.
	s.Registry.Permissions.LoadPermissionsFromDB(
		[]*doctype.Role{{Name: "Administrator"}, {Name: "Creator"}},
		[]*doctype.Permission{{Doctype: "Task", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true, Submit: true}},
	)
	retry := exec(t, s, k, op)
	if retry.Error == nil || retry.Error.Type != contract.CodePermissionDenied || retry.Replayed {
		t.Fatalf("revoked actor replay result=%#v error=%#v; want permission denied without replay", retry, retry.Error)
	}
	if s.taskCount(t) != 1 {
		t.Fatalf("revoked retry changed task count to %d", s.taskCount(t))
	}
}

func TestIdempotencyScopeIncludesPrincipalAndCommand(t *testing.T) {
	s := newSite(t, "idempotency-scope")
	defer s.DB.Close()
	k := newKernel(s)
	key := "shared-client-key"

	// The same user-visible key and byte-identical payload represent distinct
	// operations for distinct verified principals.
	alice := kernel.Operation{Command: kernel.CommandRecordCreate, Payload: json.RawMessage(`{"doctype":"Task","data":{"title":"Alice"}}`), Context: opCtx(s.Name, "Administrator")}
	alice.Context.IdempotencyKey = key
	alice.Context.Actor.PrincipalID = "alice"
	alice.Context.User = "alice"
	alice.Context.Execution, _ = policy.NewVerifiedExecution(contract.TenantID(s.Name), alice.Context.Actor)
	mustComplete(t, exec(t, s, k, alice))
	aliceRetry := alice
	aliceRetry.Payload = json.RawMessage("{ \"data\": { \"title\": \"Alice\" }, \"doctype\": \"Task\" }\n")
	canonicalReplay := exec(t, s, k, aliceRetry)
	mustComplete(t, canonicalReplay)
	if !canonicalReplay.Replayed || s.taskCount(t) != 1 {
		t.Fatalf("equivalent JSON formatting should replay Alice's receipt: replay=%v records=%d", canonicalReplay.Replayed, s.taskCount(t))
	}

	bob := alice
	bob.Context = opCtx(s.Name, "Administrator")
	bob.Context.Actor.PrincipalID = "bob"
	bob.Context.User = "bob"
	bob.Context.Execution, _ = policy.NewVerifiedExecution(contract.TenantID(s.Name), bob.Context.Actor)
	bobResult := exec(t, s, k, bob)
	mustComplete(t, bobResult)
	if bobResult.Replayed || s.taskCount(t) != 2 {
		t.Fatalf("another principal's receipt was reused: replay=%v records=%d", bobResult.Replayed, s.taskCount(t))
	}

	// Reuse the same key, actor, and raw payload for a different command. The
	// payload intentionally contains both command shapes; command identity is
	// part of the receipt scope.
	raw := json.RawMessage(`{"doctype":"Task","data":{"title":"create-shape"},"records":[{"key":"bundle","doctype":"Task","data":{"title":"bundle-shape"}}]}`)
	create := kernel.Operation{Command: kernel.CommandRecordCreate, Payload: raw, Context: opCtx(s.Name, "Administrator")}
	create.Context.IdempotencyKey = "cross-command-key"
	mustComplete(t, exec(t, s, k, create))
	bundle := kernel.Operation{Command: kernel.CommandRecordMutateBundle, Payload: raw, Context: opCtx(s.Name, "Administrator")}
	bundle.Context.IdempotencyKey = "cross-command-key"
	bundleResult := exec(t, s, k, bundle)
	mustComplete(t, bundleResult)
	if bundleResult.Replayed || s.taskCount(t) != 4 {
		t.Fatalf("another command's receipt was reused: replay=%v records=%d", bundleResult.Replayed, s.taskCount(t))
	}
	retry := exec(t, s, k, bundle)
	mustComplete(t, retry)
	if !retry.Replayed || s.taskCount(t) != 4 {
		t.Fatalf("same scoped command retry should replay once: replay=%v records=%d", retry.Replayed, s.taskCount(t))
	}
}

func TestConcurrentSameKeyCreateCommitsExactlyOnce(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	op := createOp(map[string]any{"title": "concurrent retry"}, withKey("concurrent-create-key"))
	op.Context = opCtx(s.Name, "Administrator")
	op.Context.IdempotencyKey = "concurrent-create-key"
	start := make(chan struct{})
	type execution struct {
		result contract.CommandResult
		err    *contract.Error
	}
	results := make(chan execution, 2)
	for range 2 {
		go func() {
			<-start
			result, err := k.Execute(context.Background(), s.DB, s.Registry, op)
			results <- execution{result: result, err: err}
		}()
	}
	close(start)
	one, two := <-results, <-results
	for _, outcome := range []execution{one, two} {
		if outcome.err != nil {
			t.Fatalf("concurrent retry failed: %v", outcome.err)
		}
		if outcome.result.Status != contract.StatusCompleted {
			t.Fatalf("concurrent retry status = %s; expected completed", outcome.result.Status)
		}
	}
	if string(one.result.Data) != string(two.result.Data) {
		t.Fatalf("concurrent same-key calls returned different results: %s / %s", one.result.Data, two.result.Data)
	}
	if one.result.Replayed == two.result.Replayed {
		t.Fatalf("expected one original result and one replay, got replay flags %t/%t", one.result.Replayed, two.result.Replayed)
	}
	if got := s.taskCount(t); got != 1 {
		t.Fatalf("records created = %d, want exactly one", got)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name='record.create' AND status='completed'", s.Name); got != 1 {
		t.Fatalf("completed audits = %d, want exactly one", got)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site=? AND aggregate_type='Task'", s.Name); got != 1 {
		t.Fatalf("outbox events = %d, want exactly one", got)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name='record.create'", s.Name); got != 1 {
		t.Fatalf("idempotency receipts = %d, want exactly one", got)
	}
}

func TestConcurrentSameKeyMutationBundleCommitsExactlyOnce(t *testing.T) {
	s := newSite(t, "site-bundle-concurrent-retry")
	defer s.DB.Close()
	k := newKernel(s)

	rootData, err := json.Marshal(map[string]any{"title": "bundle root"})
	if err != nil {
		t.Fatal("marshal root data:", err)
	}
	childData, err := json.Marshal(map[string]any{"title": "bundle child"})
	if err != nil {
		t.Fatal("marshal child data:", err)
	}
	op := bundleOp(kernel.RecordMutationBundlePayload{
		Doctype: "Task",
		Records: []kernel.RecordMutationBundleItem{
			{Key: "root", Doctype: "Task", Data: rootData},
			{Key: "child", Doctype: "Task", Data: childData},
		},
	}, withRoles("Administrator"), withKey("concurrent-bundle-retry"))
	op.Context = opCtx(s.Name, "Administrator")
	op.Context.IdempotencyKey = "concurrent-bundle-retry"

	start := make(chan struct{})
	type execution struct {
		result contract.CommandResult
		err    *contract.Error
	}
	results := make(chan execution, 2)
	for range 2 {
		go func() {
			<-start
			result, execErr := k.Execute(context.Background(), s.DB, s.Registry, op)
			results <- execution{result: result, err: execErr}
		}()
	}
	close(start)
	first, second := <-results, <-results
	for _, outcome := range []execution{first, second} {
		if outcome.err != nil {
			t.Fatalf("concurrent bundle retry failed: %v", outcome.err)
		}
		if outcome.result.Status != contract.StatusCompleted {
			t.Fatalf("concurrent bundle status = %s; want completed", outcome.result.Status)
		}
	}
	if string(first.result.Data) != string(second.result.Data) {
		t.Fatalf("concurrent bundle calls returned different results: %s / %s", first.result.Data, second.result.Data)
	}
	if first.result.Replayed == second.result.Replayed {
		t.Fatalf("expected one original result and one replay, got replay flags %t/%t", first.result.Replayed, second.result.Replayed)
	}
	if got := s.taskCount(t); got != 2 {
		t.Fatalf("bundle records=%d, want exactly two", got)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name='record.mutate_bundle' AND status='completed'", s.Name); got != 2 {
		t.Fatalf("completed bundle audit rows=%d, want one per record (2)", got)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site=?", s.Name); got != 2 {
		t.Fatalf("bundle outbox events=%d, want exactly two", got)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name='record.mutate_bundle'", s.Name); got != 1 {
		t.Fatalf("bundle receipts=%d, want exactly one", got)
	}
}

// TestRecordDeleteUsesCanonicalPipeline verifies deletion has the same durable
// operation guarantees as create/update and can be retried safely.
func TestRecordDeleteUsesCanonicalPipeline(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	created := exec(t, s, k, createOp(map[string]any{"title": "Remove me"}, withKey("create-delete-target")))
	mustComplete(t, created)
	var createdData kernel.ResultData
	if err := json.Unmarshal(created.Data, &createdData); err != nil {
		t.Fatalf("decode create result: %v", err)
	}

	deleteOp := payloadOp("record.delete", createdData.Name, nil, withKey("delete-once"))
	deleted := exec(t, s, k, deleteOp)
	mustComplete(t, deleted)
	if n := s.taskCount(t); n != 0 {
		t.Fatalf("delete must remove the record, remaining=%d", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.delete' AND doc_name = ? AND status = 'completed'", s.Name, createdData.Name); n != 1 {
		t.Fatalf("expected one completed delete audit, got %d", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.delete'", s.Name); n != 1 {
		t.Fatalf("expected one delete receipt, got %d", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", s.Name, createdData.Name); n != 2 {
		t.Fatalf("expected create and delete events, got %d outbox rows", n)
	}

	replayed := exec(t, s, k, deleteOp)
	mustComplete(t, replayed)
	if !replayed.Replayed {
		t.Fatal("repeated delete must return the committed result")
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.delete' AND doc_name = ? AND status = 'completed'", s.Name, createdData.Name); n != 1 {
		t.Fatalf("delete replay wrote another audit record, got %d", n)
	}
}

func TestRecordMutationBundleCommitsRelatedCreateAndUpdateOnce(t *testing.T) {
	s := newSite(t, "site-mutation-bundle")
	defer s.DB.Close()
	k := newKernel(s)

	seed := exec(t, s, k, createOp(map[string]any{"title": "Existing"}))
	mustComplete(t, seed)
	var existing kernel.ResultData
	if err := json.Unmarshal(seed.Data, &existing); err != nil {
		t.Fatal("decode seed result:", err)
	}
	firstData, _ := json.Marshal(map[string]any{"title": "$records.created.name"})
	newData, _ := json.Marshal(map[string]any{"title": "Created in bundle"})
	updateData, _ := json.Marshal(map[string]any{"title": "$records.first.name"})
	operation := bundleOp(kernel.RecordMutationBundlePayload{
		Doctype: "Task",
		Records: []kernel.RecordMutationBundleItem{
			{Key: "first", Doctype: "Task", Data: firstData},
			{Key: "created", Doctype: "Task", Data: newData},
			{Key: "updated", Operation: "update", Doctype: "Task", Name: existing.Name, Data: updateData},
		},
	}, withRoles("Administrator"), withKey("bundle-create-update"))
	result := exec(t, s, k, operation)
	mustComplete(t, result)
	var bundle kernel.ResultData
	if err := json.Unmarshal(result.Data, &bundle); err != nil {
		t.Fatal("decode bundle result:", err)
	}
	if !bundle.Created || bundle.Name == "" || len(bundle.Related) != 2 || bundle.Related[0].Key != "created" || !bundle.Related[0].Created || bundle.Related[1].Key != "updated" || bundle.Related[1].Created {
		t.Fatalf("unexpected bundle result: %+v", bundle)
	}
	var forwardTitle string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", bundle.Name).Scan(&forwardTitle); err != nil {
		t.Fatal("read forward-referenced record:", err)
	}
	if forwardTitle != bundle.Related[0].Name {
		t.Fatalf("forward bundle reference resolved to %q, want generated later name %q", forwardTitle, bundle.Related[0].Name)
	}
	var updatedTitle string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", existing.Name).Scan(&updatedTitle); err != nil {
		t.Fatal("read updated record:", err)
	}
	if updatedTitle != bundle.Name {
		t.Fatalf("bundle reference resolved to %q, want generated name %q", updatedTitle, bundle.Name)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `tabTask`"); n != 3 {
		t.Fatalf("bundle committed %d records, want 3", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND operation_id = ? AND status = 'completed'", s.Name, bundle.Operation); n != 3 {
		t.Fatalf("bundle operation audit rows=%d, want one per changed record", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", s.Name); n != 4 {
		t.Fatalf("bundle + seed outbox rows=%d, want 4", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.mutate_bundle'", s.Name); n != 1 {
		t.Fatalf("bundle receipt rows=%d, want 1", n)
	}

	replay := exec(t, s, k, operation)
	mustComplete(t, replay)
	if !replay.Replayed || string(replay.Data) != string(result.Data) {
		t.Fatalf("bundle replay did not return original result: replayed=%v data=%s original=%s", replay.Replayed, replay.Data, result.Data)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `tabTask`"); n != 3 {
		t.Fatalf("bundle replay duplicated records: %d", n)
	}
	changedPayload := kernel.RecordMutationBundlePayload{
		Doctype: "Task",
		Records: []kernel.RecordMutationBundleItem{
			{Key: "first", Doctype: "Task", Data: json.RawMessage(`{"title":"different"}`)},
			{Key: "created", Doctype: "Task", Data: newData},
			{Key: "updated", Operation: "update", Doctype: "Task", Name: existing.Name, Data: updateData},
		},
	}
	reused := exec(t, s, k, bundleOp(changedPayload, withRoles("Administrator"), withKey("bundle-create-update")))
	if reused.Error == nil || reused.Error.Type != contract.CodeIdempotencyKeyReused {
		t.Fatalf("changed bundle payload should conflict on reused key, got %+v", reused.Error)
	}
}

func TestRecordMutationBundleRejectsUnauthorizedRelatedRecord(t *testing.T) {
	s := newSite(t, "site-bundle-auth")
	defer s.DB.Close()
	k := newKernel(s)
	seed := exec(t, s, k, createOp(map[string]any{"title": "Protected"}))
	mustComplete(t, seed)
	var record kernel.ResultData
	if err := json.Unmarshal(seed.Data, &record); err != nil {
		t.Fatal("decode record result:", err)
	}
	newData, _ := json.Marshal(map[string]any{"title": "must roll back"})
	updateData, _ := json.Marshal(map[string]any{"title": "unauthorized update"})
	operation := bundleOp(kernel.RecordMutationBundlePayload{
		Doctype: "Task",
		Records: []kernel.RecordMutationBundleItem{
			{Key: "new", Doctype: "Task", Data: newData},
			{Key: "protected", Operation: "update", Doctype: "Task", Name: record.Name, Data: updateData},
		},
	}, withUser("creator"), withRoles("Creator"))
	result := exec(t, s, k, operation)
	if result.Error == nil || result.Error.Type != contract.CodePermissionDenied {
		t.Fatalf("bundle related-record denial = status %s error %+v", result.Status, result.Error)
	}
	if n := s.taskCount(t); n != 1 {
		t.Fatalf("unauthorized bundle created a record, count=%d", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", s.Name); n != 1 {
		t.Fatalf("unauthorized bundle added outbox events, count=%d", n)
	}
}

func TestEntityScopedKernelAndBundleRejectForgedCrossEntityRecords(t *testing.T) {
	s := newSite(t, "site-entity-scope")
	defer s.DB.Close()
	dt := &doctype.DocType{
		Name: "Invoice", Module: "Accounting", BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id",
		Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "legal_entity_id", Fieldtype: "Data", Reqd: true}},
	}
	s.Registry.Register(dt)
	if err := schema.MigrateSiteFromRegistry(s.DB, s.Name, s.Registry, s.Dialect); err != nil {
		t.Fatal("migrate entity-owned invoice:", err)
	}
	scopeA := contract.BusinessScope{TenantID: contract.TenantID(s.Name), Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	actor := contract.ActorContext{PrincipalID: "scheduler", PrincipalType: contract.PrincipalService, Site: s.Name, AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(s.Name), actor, []policy.Grant{{Resource: "Invoice", Operation: "create", Scope: scopeA}})
	if err != nil {
		t.Fatal("create scoped service execution:", err)
	}
	opContext := kernel.OperationContext{Site: s.Name, TenantID: contract.TenantID(s.Name), Actor: actor, Execution: execution, BusinessScope: scopeA, Source: kernel.SourceWorker}
	forgedPayload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: "Invoice", Data: json.RawMessage(`{"title":"forged","legal_entity_id":"entity-b"}`)})
	forged, _ := newKernel(s).Execute(context.Background(), s.DB, s.Registry, kernel.Operation{Command: kernel.CommandRecordCreate, Payload: forgedPayload, Context: opContext})
	if forged.Error == nil {
		t.Fatalf("forged entity insert = status %s error %+v, want fail-closed rejection", forged.Status, forged.Error)
	}
	first, _ := json.Marshal(map[string]any{"title": "would otherwise insert"})
	second, _ := json.Marshal(map[string]any{"title": "cross entity", "legal_entity_id": "entity-b"})
	bundle := kernel.RecordMutationBundlePayload{Doctype: "Invoice", Records: []kernel.RecordMutationBundleItem{{Key: "first", Doctype: "Invoice", Data: first}, {Key: "second", Doctype: "Invoice", Data: second}}}
	bundlePayload, _ := json.Marshal(bundle)
	bundleResult, _ := newKernel(s).Execute(context.Background(), s.DB, s.Registry, kernel.Operation{Command: kernel.CommandRecordMutateBundle, Payload: bundlePayload, Context: opContext})
	if bundleResult.Error == nil {
		t.Fatalf("cross-entity bundle = status %s error %+v, want atomic fail-closed rejection", bundleResult.Status, bundleResult.Error)
	}
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM `tabInvoice`").Scan(&count); err != nil {
		t.Fatal("count rejected invoice mutations:", err)
	}
	if count != 0 {
		t.Fatalf("forged operations committed %d invoices, want none", count)
	}
}

func TestRegisteredCrossEntityCommandCommitsOnlyWithBothEntityGrants(t *testing.T) {
	s := newSite(t, "site-cross-entity-command")
	defer s.DB.Close()
	k := newKernel(s)
	dt := &doctype.DocType{
		Name: "Transfer", Module: "Finance",
		Fields:              []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "legal_entity_id", Fieldtype: "Data", Reqd: true}},
		BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id",
	}
	s.Registry.Register(dt)
	if err := schema.MigrateSiteFromRegistry(s.DB, s.Name, s.Registry, s.Dialect); err != nil {
		t.Fatal("migrate entity-owned transfer:", err)
	}
	k.Commands = kernel.NewCommandRegistry()
	command := `
name: transfer.apply
namespace: finance
version: 1
input:
  record: Transfer
cross_entity:
  resource: Intercompany Transfer
  operation: write
  target_entity_field: input.target_entity_id
transaction:
  - create:
      record: Transfer
      business_scope: source
      values:
        title: $input.source_title
  - create:
      record: Transfer
      business_scope: target
      values:
        title: $input.target_title
`
	if _, err := kernel.ParseCommandResource([]byte(command)); err != nil {
		t.Fatal("parse cross-entity command:", err)
	}
	mustRegister(t, k, command)
	source := contract.BusinessScope{TenantID: contract.TenantID(s.Name), Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	target := contract.BusinessScope{TenantID: contract.TenantID(s.Name), Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	actor := contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: s.Name, AuthenticatedAt: time.Now().UTC()}
	baseGrants := []policy.Grant{
		{Resource: "Intercompany Transfer", Operation: "write", Scope: source},
		{Resource: "Intercompany Transfer", Operation: "write", Scope: target},
		{Resource: "Transfer", Operation: "create", Scope: source},
		{Resource: "Transfer", Operation: "create", Scope: target},
	}
	makeOperation := func(grants []policy.Grant) kernel.Operation {
		execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(s.Name), actor, grants)
		if err != nil {
			t.Fatal("create verified execution:", err)
		}
		payload, _ := json.Marshal(map[string]any{"data": map[string]any{
			"source_entity_id": "entity-c", "target_entity_id": "entity-b", "source_title": "source side", "target_title": "target side",
		}})
		return kernel.Operation{Command: "finance.transfer.apply", Payload: payload, Context: kernel.OperationContext{
			Site: s.Name, TenantID: contract.TenantID(s.Name), Actor: actor, Execution: execution, BusinessScope: source,
		}}
	}
	denied, denialErr := k.Execute(context.Background(), s.DB, s.Registry, makeOperation(baseGrants[:3]))
	if denialErr == nil || denialErr.Type != contract.CodePermissionDenied {
		t.Fatalf("operation without target record grant = result %+v, err %v; want permission denied", denied, denialErr)
	}
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM `tabTransfer`").Scan(&count); err != nil {
		t.Fatal("count denied transfer records:", err)
	}
	if count != 0 {
		t.Fatalf("denied cross-entity command committed %d records", count)
	}

	result, err := k.Execute(context.Background(), s.DB, s.Registry, makeOperation(baseGrants))
	if err != nil || result.Error != nil || result.Status != contract.StatusCompleted {
		t.Fatalf("dual-authorized registered command = result %+v, err type=%v message=%q", result, err.Type, err.Message)
	}
	rows, queryErr := s.DB.Query("SELECT legal_entity_id, title FROM `tabTransfer` ORDER BY legal_entity_id")
	if queryErr != nil {
		t.Fatal("load cross-entity command effects:", queryErr)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var entityID, title string
		if err := rows.Scan(&entityID, &title); err != nil {
			t.Fatal("scan cross-entity command effect:", err)
		}
		got = append(got, entityID+":"+title)
	}
	if err := rows.Err(); err != nil {
		t.Fatal("iterate cross-entity command effects:", err)
	}
	if len(got) != 2 || got[0] != "entity-a:source side" || got[1] != "entity-b:target side" {
		t.Fatalf("cross-entity command effects = %#v; want one correctly scoped record per entity", got)
	}
}

func TestRecordMutationBundleRollsBackEarlierWritesOnSQLFailure(t *testing.T) {
	s := newSite(t, "site-bundle-rollback")
	defer s.DB.Close()
	k := newKernel(s)
	existingResult := exec(t, s, k, createOp(map[string]any{"title": "keep unchanged"}))
	mustComplete(t, existingResult)
	duplicateResult := exec(t, s, k, createOp(map[string]any{"title": "unique seed", "serial": "DUPLICATE"}))
	mustComplete(t, duplicateResult)
	var existing kernel.ResultData
	if err := json.Unmarshal(existingResult.Data, &existing); err != nil {
		t.Fatal("decode existing result:", err)
	}
	updateData, _ := json.Marshal(map[string]any{"title": "must roll back"})
	duplicateData, _ := json.Marshal(map[string]any{"title": "must not insert", "serial": "DUPLICATE"})
	result := exec(t, s, k, bundleOp(kernel.RecordMutationBundlePayload{
		Doctype: "Task",
		Records: []kernel.RecordMutationBundleItem{
			{Key: "update", Operation: "update", Doctype: "Task", Name: existing.Name, Data: updateData},
			{Key: "duplicate", Doctype: "Task", Data: duplicateData},
		},
	}, withRoles("Administrator"), withKey("bundle-rollback")))
	if result.Error == nil || result.Error.Type != contract.CodeConflict {
		t.Fatalf("unique violation bundle result = status %s error %+v, want conflict", result.Status, result.Error)
	}
	var title string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", existing.Name).Scan(&title); err != nil {
		t.Fatal("read rolled-back update:", err)
	}
	if title != "keep unchanged" || s.taskCount(t) != 2 {
		t.Fatalf("failed bundle left partial business data: title=%q task_count=%d", title, s.taskCount(t))
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.mutate_bundle' AND status = 'completed'", s.Name); n != 0 {
		t.Fatalf("failed bundle committed %d audit rows", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", s.Name); n != 2 {
		t.Fatalf("failed bundle changed outbox count to %d, want only two seed events", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.mutate_bundle'", s.Name); n != 0 {
		t.Fatalf("failed bundle committed an idempotency receipt")
	}
}

func TestRecordMutationBundleRejectsMalformedReferencesAndDuplicateKeys(t *testing.T) {
	s := newSite(t, "site-bundle-invalid")
	defer s.DB.Close()
	k := newKernel(s)
	unknownRef, _ := json.Marshal(map[string]any{"title": "$records.missing.name"})
	unknown := bundleOp(kernel.RecordMutationBundlePayload{
		Doctype: "Task", Records: []kernel.RecordMutationBundleItem{{Key: "record", Doctype: "Task", Data: unknownRef}},
	}, withRoles("Administrator"))
	if result := exec(t, s, k, unknown); result.Error == nil || result.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("unknown reference result = %+v", result.Error)
	}
	data, _ := json.Marshal(map[string]any{"title": "duplicate key"})
	duplicate := bundleOp(kernel.RecordMutationBundlePayload{
		Doctype: "Task", Records: []kernel.RecordMutationBundleItem{
			{Key: "same", Doctype: "Task", Data: data}, {Key: "same", Doctype: "Task", Data: data},
		},
	}, withRoles("Administrator"))
	if result := exec(t, s, k, duplicate); result.Error == nil || result.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("duplicate key result = %+v", result.Error)
	}
	tooMany := kernel.RecordMutationBundlePayload{Doctype: "Task"}
	for index := 0; index <= kernel.MaxMutationBundleRecords; index++ {
		tooMany.Records = append(tooMany.Records, kernel.RecordMutationBundleItem{Key: fmt.Sprintf("record-%02d", index), Doctype: "Task", Data: data})
	}
	if result := exec(t, s, k, bundleOp(tooMany, withRoles("Administrator"))); result.Error == nil || result.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("oversized bundle result = %+v", result.Error)
	}
	if n := s.taskCount(t); n != 0 {
		t.Fatalf("invalid bundles wrote %d records", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.mutate_bundle' AND status = 'completed'", s.Name); n != 0 {
		t.Fatalf("invalid bundle wrote %d completed audits", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", s.Name); n != 0 {
		t.Fatalf("invalid bundle wrote %d outbox events", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ?", s.Name); n != 0 {
		t.Fatalf("invalid bundle wrote %d receipts", n)
	}
}

func TestRecordMutationBundleRejectsStaleRootVersion(t *testing.T) {
	s := newSite(t, "site-bundle-version")
	defer s.DB.Close()
	k := newKernel(s)
	created := exec(t, s, k, createOp(map[string]any{"title": "Concurrent"}))
	mustComplete(t, created)
	var record kernel.ResultData
	if err := json.Unmarshal(created.Data, &record); err != nil {
		t.Fatal("decode record result:", err)
	}
	updateData, _ := json.Marshal(map[string]any{"title": "must not win"})
	operation := bundleOp(kernel.RecordMutationBundlePayload{
		Doctype: "Task",
		Records: []kernel.RecordMutationBundleItem{{Key: "root", Operation: "update", Doctype: "Task", Name: record.Name, Data: updateData}},
	}, withRoles("Administrator"), withExpectedVersion("stale-version"))
	result := exec(t, s, k, operation)
	if result.Error == nil || result.Error.Type != contract.CodeConflict {
		t.Fatalf("stale bundle expected-version = status %s error %+v, want conflict", result.Status, result.Error)
	}
	var title string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", record.Name).Scan(&title); err != nil {
		t.Fatal("read unchanged bundle record:", err)
	}
	if title != "Concurrent" {
		t.Fatalf("stale bundle changed title to %q", title)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND command_name = 'record.mutate_bundle' AND status = 'completed'", s.Name); n != 0 {
		t.Fatalf("stale bundle committed %d completion audits", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", s.Name); n != 1 {
		t.Fatalf("stale bundle changed outbox count to %d, want only the seed create event", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ?", s.Name); n != 0 {
		t.Fatalf("stale bundle created a receipt, receipts=%d", n)
	}
}

func TestConcurrentMutationBundleRejectsStaleRelatedRevision(t *testing.T) {
	s := newSite(t, "site-bundle-racing-related-update")
	defer s.DB.Close()
	k := newKernel(s)
	created := exec(t, s, k, createOp(map[string]any{"title": "Inventory balance"}))
	mustComplete(t, created)
	var record kernel.ResultData
	if err := json.Unmarshal(created.Data, &record); err != nil {
		t.Fatal("decode create result:", err)
	}
	var expectedRevision uint64
	if err := s.DB.QueryRow("SELECT revision FROM `tabTask` WHERE name = ?", record.Name).Scan(&expectedRevision); err != nil {
		t.Fatal("load related record revision:", err)
	}

	start := make(chan struct{})
	type execution struct {
		result contract.CommandResult
		err    *contract.Error
	}
	results := make(chan execution, 2)
	for index, title := range []string{"first bundle update", "second bundle update"} {
		payload := kernel.RecordMutationBundlePayload{Doctype: "Task", Records: []kernel.RecordMutationBundleItem{{
			Key: "balance", Operation: "update", Doctype: "Task", Name: record.Name,
			ExpectedRevision: expectedRevision, Data: json.RawMessage(fmt.Sprintf(`{"title":%q}`, title)),
		}}}
		op := bundleOp(payload, withRoles("Administrator"), withKey(fmt.Sprintf("bundle-related-update-%d", index)))
		op.Context = opCtx(s.Name, "Administrator")
		op.Context.IdempotencyKey = fmt.Sprintf("bundle-related-update-%d", index)
		go func() {
			<-start
			result, err := k.Execute(context.Background(), s.DB, s.Registry, op)
			results <- execution{result: result, err: err}
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			if outcome.err.Type == contract.CodeConflict {
				conflicts++
				continue
			}
			t.Fatalf("concurrent related-record update failed unexpectedly: %v", outcome.err)
		}
		switch {
		case outcome.result.Error == nil && outcome.result.Status == contract.StatusCompleted:
			successes++
		case outcome.result.Error != nil && outcome.result.Error.Type == contract.CodeConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent related update: status=%s error=%+v", outcome.result.Status, outcome.result.Error)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent bundle revision results: successes=%d conflicts=%d; want 1 each", successes, conflicts)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name='record.mutate_bundle' AND status='completed'", s.Name); got != 1 {
		t.Fatalf("completed bundle audits = %d, want exactly one", got)
	}
}

func TestRecordWorkflowTransitionUsesCanonicalPipeline(t *testing.T) {
	s := newSite(t, "site-workflow")
	defer s.DB.Close()
	s.StatusPolicies.Set(workflow.StatusPolicy{
		Table: "Task", StateField: "status",
		InitialState: "Open",
		Transitions:  []workflow.StatusTransition{{Action: "Complete", From: "Open", To: "Done", DocStatus: 1, AllowedRoles: []string{"Administrator"}, RequireFields: []string{"title"}}},
	})
	k := newKernel(s)
	created := exec(t, s, k, createOp(map[string]any{"title": "Finish this", "status": "Open"}))
	mustComplete(t, created)
	var docName string
	if err := s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title = ?", "Finish this").Scan(&docName); err != nil {
		t.Fatal("load task name:", err)
	}
	payload, err := json.Marshal(map[string]any{"doctype": "Task", "name": docName, "action": "Complete"})
	if err != nil {
		t.Fatal(err)
	}
	op := kernel.Operation{Command: kernel.CommandWorkflowStatusTransition, Payload: payload}
	op.Context.Site = s.Name
	op.Context.User = "Administrator"
	op.Context.Roles = []string{"Administrator"}
	op.Context.Actor = contract.ActorContext{PrincipalID: "Administrator", PrincipalType: contract.PrincipalHuman, Site: s.Name, Roles: op.Context.Roles, AuthenticatedAt: time.Now()}
	op.Context.Execution, _ = policy.NewVerifiedExecution(contract.TenantID(s.Name), op.Context.Actor)
	op.Context.TenantID = contract.TenantID(s.Name)
	result, cerr := k.Execute(context.Background(), s.DB, s.Registry, op)
	if cerr != nil || result.Status != contract.StatusCompleted {
		t.Fatalf("workflow command = status %q error %+v", result.Status, cerr)
	}
	var state string
	var docStatus int
	if err := s.DB.QueryRow("SELECT status, doc_status FROM `tabTask` WHERE name = ?", docName).Scan(&state, &docStatus); err != nil {
		t.Fatal("read transitioned task:", err)
	}
	if state != "Done" || docStatus != 1 {
		t.Fatalf("transitioned task = state %q doc_status %d, want Done/1", state, docStatus)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND command_name = ?", s.Name, docName, kernel.CommandWorkflowStatusTransition); n != 1 {
		t.Fatalf("workflow audit rows = %d, want 1", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", s.Name, docName); n != 2 {
		t.Fatalf("create + transition outbox rows = %d, want one change event per mutation", n)
	}
}

func TestGenericRecordWritesCannotBypassActiveWorkflow(t *testing.T) {
	s := newSite(t, "site-workflow-protected-state")
	defer s.DB.Close()
	s.StatusPolicies.Set(workflow.StatusPolicy{
		Table: "Task", StateField: "status",
		InitialState: "Open",
		Transitions:  []workflow.StatusTransition{{Action: "Complete", From: "Open", To: "Done", DocStatus: 1, AllowedRoles: []string{"Administrator"}}},
	})
	k := newKernel(s)
	createdTerminal := createOp(map[string]any{"title": "Must not start completed", "status": "Done"}, withRoles("Administrator"), withKey("protected-workflow-create"))
	createdTerminal.Context = opCtx(s.Name, "Administrator")
	createdTerminal.Context.IdempotencyKey = "protected-workflow-create"
	_, createErr := k.Execute(context.Background(), s.DB, s.Registry, createdTerminal)
	if createErr == nil || createErr.Type != contract.CodeValidationFailed {
		t.Fatalf("generic record.create terminal state error = %#v, want validation failure", createErr)
	}
	bundleCreate := bundleOp(kernel.RecordMutationBundlePayload{Doctype: "Task", Records: []kernel.RecordMutationBundleItem{{
		Key: "task", Doctype: "Task", Data: json.RawMessage(`{"title":"Must not start completed","status":"Done"}`),
	}}}, withRoles("Administrator"), withKey("protected-workflow-bundle-create"))
	bundleCreate.Context = opCtx(s.Name, "Administrator")
	bundleCreate.Context.IdempotencyKey = "protected-workflow-bundle-create"
	_, bundleCreateErr := k.Execute(context.Background(), s.DB, s.Registry, bundleCreate)
	if bundleCreateErr == nil || bundleCreateErr.Type != contract.CodeValidationFailed {
		t.Fatalf("bundle create terminal state error = %#v, want validation failure", bundleCreateErr)
	}
	created := exec(t, s, k, createOp(map[string]any{"title": "Protected state", "status": "Open"}))
	mustComplete(t, created)
	var name string
	if err := s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title = ?", "Protected state").Scan(&name); err != nil {
		t.Fatal("load seeded task:", err)
	}

	updatePayload, _ := json.Marshal(kernel.RecordUpdatePayload{Doctype: "Task", Name: name, Data: json.RawMessage(`{"status":"Done"}`)})
	_, updateErr := k.Execute(context.Background(), s.DB, s.Registry, kernel.Operation{
		Command: kernel.CommandRecordUpdate, Payload: updatePayload, Context: opCtx(s.Name, "Administrator"),
	})
	if updateErr == nil || updateErr.Type != contract.CodeValidationFailed {
		t.Fatalf("generic record.update error = %#v, want validation failure", updateErr)
	}

	bundlePayload := kernel.RecordMutationBundlePayload{Doctype: "Task", Records: []kernel.RecordMutationBundleItem{{
		Key: "task", Operation: "update", Doctype: "Task", Name: name, Data: json.RawMessage(`{"status":"Done"}`),
	}}}
	bundleOperation := bundleOp(bundlePayload)
	bundleOperation.Context = opCtx(s.Name, "Administrator")
	bundleOperation.Context.IdempotencyKey = "protected-workflow-state"
	_, bundleErr := k.Execute(context.Background(), s.DB, s.Registry, bundleOperation)
	if bundleErr == nil || bundleErr.Type != contract.CodeValidationFailed {
		t.Fatalf("bundle state update error = %#v, want validation failure", bundleErr)
	}
	var state string
	if err := s.DB.QueryRow("SELECT status FROM `tabTask` WHERE name = ?", name).Scan(&state); err != nil {
		t.Fatal("read task state:", err)
	}
	if state != "Open" {
		t.Fatalf("generic writes changed workflow state to %q; want Open", state)
	}
}

func TestMutationBundleRunsAuthorizedWorkflowTransitionAtomically(t *testing.T) {
	s := newSite(t, "site-bundle-workflow")
	defer s.DB.Close()
	s.StatusPolicies.Set(workflow.StatusPolicy{
		Table: "Task", StateField: "status",
		InitialState: "Open",
		Transitions:  []workflow.StatusTransition{{Action: "Complete", From: "Open", To: "Done", DocStatus: 1, AllowedRoles: []string{"Administrator"}, RequireFields: []string{"title"}}},
	})
	k := newKernel(s)
	created := exec(t, s, k, createOp(map[string]any{"title": "Complete with a related record", "status": "Open"}))
	mustComplete(t, created)
	var original kernel.ResultData
	if err := json.Unmarshal(created.Data, &original); err != nil {
		t.Fatal("decode created record:", err)
	}
	childData, _ := json.Marshal(map[string]any{"title": "$records.root.name"})
	bundle := kernel.RecordMutationBundlePayload{Doctype: "Task", Records: []kernel.RecordMutationBundleItem{
		{Key: "root", Operation: "update", Doctype: "Task", Name: original.Name, TransitionAction: "Complete", Data: json.RawMessage(`{"title":"Completed","related_task":"$records.related.name"}`)},
		{Key: "related", Doctype: "Task", Data: childData},
	}}
	op := bundleOp(bundle, withRoles("Administrator"), withKey("bundle-workflow-complete"))
	result := exec(t, s, k, op)
	mustComplete(t, result)
	var data kernel.ResultData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal("decode bundle result:", err)
	}
	var state, title, linkedRecord string
	if err := s.DB.QueryRow("SELECT status, title, related_task FROM `tabTask` WHERE name = ?", original.Name).Scan(&state, &title, &linkedRecord); err != nil {
		t.Fatal("read transitioned record:", err)
	}
	if state != "Done" || title != "Completed" || len(data.Related) != 1 || linkedRecord != data.Related[0].Name {
		t.Fatalf("transition bundle state=%q title=%q linked=%q related=%+v", state, title, linkedRecord, data.Related)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM `tabTask`"); n != 2 {
		t.Fatalf("bundle rows=%d, want two atomic records", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", s.Name); n != 3 {
		t.Fatalf("bundle outbox rows=%d, want one change event per mutation", n)
	}
}

func TestMutationBundleRequiresSubmitPermissionForTransitionAction(t *testing.T) {
	s := newSite(t, "site-bundle-workflow-submit-denied")
	defer s.DB.Close()
	s.StatusPolicies.Set(workflow.StatusPolicy{
		Table: "Task", StateField: "status",
		InitialState: "Open",
		Transitions:  []workflow.StatusTransition{{Action: "Complete", From: "Open", To: "Done", DocStatus: 1, AllowedRoles: []string{"Writer"}}},
	})
	k := newKernel(s)
	created := exec(t, s, k, createOp(map[string]any{"title": "Protected", "status": "Open"}))
	mustComplete(t, created)
	var original kernel.ResultData
	if err := json.Unmarshal(created.Data, &original); err != nil {
		t.Fatal("decode created record:", err)
	}
	bundle := kernel.RecordMutationBundlePayload{Doctype: "Task", Records: []kernel.RecordMutationBundleItem{
		{Key: "root", Operation: "update", Doctype: "Task", Name: original.Name, TransitionAction: "Complete", Data: json.RawMessage(`{"title":"must not save"}`)},
		{Key: "related", Doctype: "Task", Data: json.RawMessage(`{"title":"must not create"}`)},
	}}
	op := bundleOp(bundle, withRoles("Writer"), withKey("bundle-workflow-denied"))
	result := exec(t, s, k, op)
	if result.Error == nil || result.Error.Type != contract.CodePermissionDenied {
		t.Fatalf("workflow bundle error = %+v, want permission denied", result.Error)
	}
	var state, title string
	if err := s.DB.QueryRow("SELECT status, title FROM `tabTask` WHERE name = ?", original.Name).Scan(&state, &title); err != nil {
		t.Fatal("read unchanged record:", err)
	}
	if state != "Open" || title != "Protected" || s.taskCount(t) != 1 {
		t.Fatalf("unauthorized bundle left effects: state=%q title=%q count=%d", state, title, s.taskCount(t))
	}
}

func TestMutationBundleRollsBackTransitionWhenRelatedWriteFails(t *testing.T) {
	s := newSite(t, "site-bundle-workflow-rollback")
	defer s.DB.Close()
	s.StatusPolicies.Set(workflow.StatusPolicy{
		Table: "Task", StateField: "status",
		InitialState: "Open",
		Transitions:  []workflow.StatusTransition{{Action: "Complete", From: "Open", To: "Done", DocStatus: 1, AllowedRoles: []string{"Administrator"}}},
	})
	k := newKernel(s)
	created := exec(t, s, k, createOp(map[string]any{"title": "Original", "status": "Open"}))
	mustComplete(t, created)
	if duplicate := exec(t, s, k, createOp(map[string]any{"title": "Unique seed", "serial": "DUPLICATE"})); duplicate.Error != nil {
		t.Fatalf("seed duplicate value: %+v", duplicate.Error)
	}
	var original kernel.ResultData
	if err := json.Unmarshal(created.Data, &original); err != nil {
		t.Fatal("decode created record:", err)
	}
	bundle := kernel.RecordMutationBundlePayload{Doctype: "Task", Records: []kernel.RecordMutationBundleItem{
		{Key: "root", Operation: "update", Doctype: "Task", Name: original.Name, TransitionAction: "Complete", Data: json.RawMessage(`{"title":"must roll back"}`)},
		{Key: "related", Doctype: "Task", Data: json.RawMessage(`{"title":"must fail","serial":"DUPLICATE"}`)},
	}}
	op := bundleOp(bundle, withRoles("Administrator"), withKey("bundle-workflow-rollback"))
	result := exec(t, s, k, op)
	if result.Error == nil || result.Error.Type != contract.CodeConflict {
		t.Fatalf("conflicting bundle error = %+v, want conflict", result.Error)
	}
	var state, title string
	if err := s.DB.QueryRow("SELECT status, title FROM `tabTask` WHERE name = ?", original.Name).Scan(&state, &title); err != nil {
		t.Fatal("read rolled-back record:", err)
	}
	if state != "Open" || title != "Original" || s.taskCount(t) != 2 {
		t.Fatalf("failed bundle left business effects: state=%q title=%q count=%d", state, title, s.taskCount(t))
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ?", s.Name); n != 2 {
		t.Fatalf("failed bundle changed outbox count to %d, want only the two committed seed events", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND command_name = 'record.mutate_bundle'", s.Name); n != 0 {
		t.Fatalf("failed bundle committed %d operation receipts", n)
	}
}

func TestPublicFormSubmitUsesConfiguredAllowlistThroughKernel(t *testing.T) {
	s := newSite(t, "site-public-form")
	defer s.DB.Close()
	dt := s.Registry.Get("Task")
	dt.PublicAccess = &doctype.PublicAccess{Enabled: true, Fields: []string{"title"}}
	s.Registry.Register(dt)
	s.Registry.Views.Register(&doctype.View{
		Name: "Public request", Route: "/request", SourceDocType: "Task",
		PublicAccess: &doctype.ViewPublicAccess{Enabled: true, AllowMutations: true},
	})
	k := newKernel(s)
	payload, err := json.Marshal(map[string]any{
		"doctype": "Task", "public_route": "/request",
		"data": map[string]any{"title": "Public ticket", "serial": "must-be-discarded"},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := kernel.Operation{Command: kernel.CommandPublicFormSubmit, Payload: payload}
	op.Context.Site = s.Name
	op.Context.User = "public"
	op.Context.Source = kernel.SourceHTTP
	op.Context.Actor = contract.ActorContext{PrincipalID: "public-form", PrincipalType: contract.PrincipalPublic, Site: s.Name, AuthenticatedAt: time.Now()}
	op.Context.Execution, err = policy.NewVerifiedExecution(contract.TenantID(s.Name), op.Context.Actor)
	if err != nil {
		t.Fatalf("create verified public-form execution: %v", err)
	}
	op.Context.TenantID = op.Context.Execution.Tenant()
	result, cerr := k.Execute(context.Background(), s.DB, s.Registry, op)
	if cerr != nil || result.Status != contract.StatusCompleted {
		t.Fatalf("public submit = status %q error %+v", result.Status, cerr)
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(result.Data, &operation); err != nil {
		t.Fatal("decode public result:", err)
	}
	var title, serial sql.NullString
	if err := s.DB.QueryRow("SELECT title, serial FROM `tabTask` WHERE name = ?", operation.Name).Scan(&title, &serial); err != nil {
		t.Fatal("read public record:", err)
	}
	if title.String != "Public ticket" || serial.Valid {
		t.Fatalf("public fields persisted title=%q serial=%#v", title.String, serial)
	}

	payload, _ = json.Marshal(map[string]any{"doctype": "Task", "public_route": "/disabled", "data": map[string]any{"title": "Denied"}})
	op.Payload = payload
	denied, cerr := k.Execute(context.Background(), s.DB, s.Registry, op)
	if cerr == nil || cerr.Type != contract.CodePermissionDenied || denied.Status != contract.StatusRejected {
		t.Fatalf("unconfigured public route must be denied, got status=%q error=%+v", denied.Status, cerr)
	}
}

// TestTenantIsolation proves operations bind tenant identity from the
// OperationContext only: writes land in the caller's database and the actor's
// site must match the executing site.
func TestTenantIsolation(t *testing.T) {
	a := newSite(t, "tenant-alpha")
	defer a.DB.Close()
	b := newSite(t, "tenant-beta")
	defer b.DB.Close()
	ka, kb := newKernel(a), newKernel(b)

	resA := exec(t, a, ka, createOp(map[string]any{"title": "Alpha doc"}))
	mustComplete(t, resA)
	resB := exec(t, b, kb, createOp(map[string]any{"title": "Beta doc"}))
	mustComplete(t, resB)

	if a.taskCount(t) != 1 || b.taskCount(t) != 1 {
		t.Fatalf("each tenant DB must contain exactly its own document")
	}

	// Cross-tenant actor mismatch is rejected fail-closed before any write.
	op := createOp(map[string]any{"title": "Smuggled"})
	op.Context.Site = b.Name // executing against B's DB...
	op.Context.Actor = contract.ActorContext{PrincipalID: "x@alpha", PrincipalType: contract.PrincipalHuman, Site: a.Name, AuthenticatedAt: time.Now()}
	res, _ := kb.Execute(context.Background(), b.DB, b.Registry, op)
	if res.Status == contract.StatusCompleted {
		t.Fatalf("cross-tenant actor must never commit into another tenant")
	}
	if res.Error == nil || (res.Error.Type != contract.CodePermissionDenied && res.Error.Type != contract.CodeUnauthenticated) {
		t.Fatalf("expected typed denial for cross-tenant actor, got %+v", res.Error)
	}
	if b.taskCount(t) != 1 {
		t.Fatalf("rejected cross-tenant operation must not write")
	}
}

// TestAuthorizationParityAcrossSources proves identical allow/deny outcomes
// regardless of adapter surface (KERNEL-004 / SEC-003 parity requirement).
func TestAuthorizationParityAcrossSources(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()

	sources := []kernel.Source{kernel.SourceHTTP, kernel.SourceSDK, kernel.SourceMCP, kernel.SourceAI, kernel.SourceCLI, kernel.SourceUI, kernel.SourceWorker, kernel.SourceIntegration}
	for _, src := range sources {
		k := newKernel(s)

		admin := exec(t, s, k, createOp(map[string]any{"title": "by " + string(src)}, withUser("root@"+string(src)), withRoles("Administrator"), withSource(src)))
		mustComplete(t, admin)

		creatorUpdate := exec(t, s, k, updateOp("TASK-0001", map[string]any{"title": "nope"}, withUser("limited@"+string(src)), withRoles("Creator"), withSource(src)))
		if creatorUpdate.Error == nil || creatorUpdate.Error.Type != contract.CodePermissionDenied {
			t.Fatalf("%s: creator lacks write; expected PERMISSION_DENIED, got %+v", src, creatorUpdate.Error)
		}

		denied := exec(t, s, k, createOp(map[string]any{"title": "x"}, withUser("norole@"+string(src)), withRoles("Nobody"), withSource(src)))
		if denied.Error == nil || denied.Error.Type != contract.CodePermissionDenied {
			t.Fatalf("%s: unknown role must be denied, got %+v", src, denied.Error)
		}

		// Exact delegated grants must follow the same operation policy for every
		// adapter source and must not turn a create grant into write permission.
		serviceExecution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(s.Name), contract.ActorContext{
			PrincipalID: "extension:" + string(src), PrincipalType: contract.PrincipalService, Site: s.Name,
		}, []policy.Grant{{Resource: "Task", Operation: "create"}})
		if err != nil {
			t.Fatalf("%s: create verified service execution: %v", src, err)
		}
		create := createOp(map[string]any{"title": "grant by " + string(src)}, withSource(src))
		create.Context.Execution = serviceExecution
		created := exec(t, s, k, create)
		mustComplete(t, created)
		var createdRecord kernel.ResultData
		if err := json.Unmarshal(created.Data, &createdRecord); err != nil {
			t.Fatalf("%s: decode delegated create: %v", src, err)
		}
		update := updateOp(createdRecord.Name, map[string]any{"title": "must remain unchanged"}, withSource(src))
		update.Context.Execution = serviceExecution
		deniedWrite := exec(t, s, k, update)
		if deniedWrite.Error == nil || deniedWrite.Error.Type != contract.CodePermissionDenied {
			t.Fatalf("%s: exact create grant authorized write: %+v", src, deniedWrite.Error)
		}
	}
	if s.taskCount(t) != len(sources)*2 {
		t.Fatalf("parity drift: expected role and exact-grant creates for each source, got %d documents", s.taskCount(t))
	}
}

// TestStaleVersionConflict proves optimistic concurrency: an update carrying
// an outdated expected_version conflicts and persists nothing.
func TestStaleVersionConflict(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	res := exec(t, s, k, createOp(map[string]any{"title": "Versioned"}))
	mustComplete(t, res)
	var docName string
	s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title='Versioned'").Scan(&docName)

	var modified time.Time
	if err := s.DB.QueryRow("SELECT modified FROM `tabTask` WHERE name = ?", docName).Scan(&modified); err != nil {
		t.Fatalf("read version: %v", err)
	}
	current := kernel.CanonicalVersion(modified)

	// Stale token → conflict, nothing changes.
	stale := exec(t, s, k, updateOp(docName, map[string]any{"title": "Should not apply"}, withExpectedVersion("2000-01-01T00:00:00Z")))
	if stale.Error == nil || stale.Error.Type != contract.CodeConflict {
		t.Fatalf("stale version must yield CONFLICT, got %+v", stale.Error)
	}
	var title string
	s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", docName).Scan(&title)
	if title != "Versioned" {
		t.Fatalf("stale operation mutated the document anyway")
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE error_code = 'CONFLICT'"); n < 1 {
		t.Fatalf("conflict attempts must leave a denial/failure audit trail")
	}

	// Current token → success.
	ok := exec(t, s, k, updateOp(docName, map[string]any{"title": "Applied"}, withExpectedVersion(current)))
	mustComplete(t, ok)
	s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", docName).Scan(&title)
	if title != "Applied" {
		t.Fatalf("correct version must apply, title=%q", title)
	}
}

func TestConcurrentExpectedVersionAllowsOnlyOneUpdate(t *testing.T) {
	s := newSite(t, "site-racing-update")
	defer s.DB.Close()
	k := newKernel(s)
	created := exec(t, s, k, createOp(map[string]any{"title": "initial"}))
	mustComplete(t, created)
	var record kernel.ResultData
	if err := json.Unmarshal(created.Data, &record); err != nil {
		t.Fatal("decode create result:", err)
	}
	var modified time.Time
	if err := s.DB.QueryRow("SELECT modified FROM `tabTask` WHERE name = ?", record.Name).Scan(&modified); err != nil {
		t.Fatal("load expected version:", err)
	}
	expected := kernel.CanonicalVersion(modified)
	start := make(chan struct{})
	type execution struct {
		result contract.CommandResult
		err    *contract.Error
	}
	results := make(chan execution, 2)
	for _, title := range []string{"first contender", "second contender"} {
		title := title
		op := updateOp(record.Name, map[string]any{"title": title}, withExpectedVersion(expected))
		op.Context = opCtx(s.Name, "Administrator")
		op.Context.ExpectedVersion = expected
		go func() {
			<-start
			result, err := k.Execute(context.Background(), s.DB, s.Registry, op)
			results <- execution{result: result, err: err}
		}()
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			if outcome.err.Type == contract.CodeConflict {
				conflicts++
				continue
			}
			t.Fatalf("concurrent version check failed unexpectedly: %v", outcome.err)
		}
		switch {
		case outcome.result.Error == nil && outcome.result.Status == contract.StatusCompleted:
			successes++
		case outcome.result.Error != nil && outcome.result.Error.Type == contract.CodeConflict:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent update result: status=%s error=%+v", outcome.result.Status, outcome.result.Error)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent expected-version results: successes=%d conflicts=%d; want 1 each", successes, conflicts)
	}
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name='record.update' AND status='completed'", s.Name); got != 1 {
		t.Fatalf("committed update audits = %d, want exactly one", got)
	}
	var finalTitle string
	if err := s.DB.QueryRow("SELECT title FROM `tabTask` WHERE name = ?", record.Name).Scan(&finalTitle); err != nil {
		t.Fatal("load final record:", err)
	}
	if finalTitle != "first contender" && finalTitle != "second contender" {
		t.Fatalf("final title = %q, neither concurrent update won", finalTitle)
	}
	var updateChanges string
	if err := s.DB.QueryRow(`SELECT h.changes_json FROM _kora_record_history h
		JOIN _kora_operation_audit a ON a.site = h.site AND a.operation_id = h.operation_id
		WHERE a.site = ? AND a.command_name = 'record.update' AND a.status = 'completed' AND a.doc_name = ?`, s.Name, record.Name).Scan(&updateChanges); err != nil {
		t.Fatalf("record update history not persisted: %v", err)
	}
	if !strings.Contains(updateChanges, `"field":"title"`) || !strings.Contains(updateChanges, "initial") {
		t.Fatalf("update history omitted before/after values: %s", updateChanges)
	}
}

// TestStaleVersionConflictLegacyToken keeps backward compatibility with the
// earlier causation-id convention while the explicit expected_version field is
// being adopted by adapters.
func TestStaleVersionConflictLegacyToken(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	res := exec(t, s, k, createOp(map[string]any{"title": "LegacyVersioned"}))
	mustComplete(t, res)
	var docName string
	s.DB.QueryRow("SELECT name FROM `tabTask` WHERE title='LegacyVersioned'").Scan(&docName)

	stale := exec(t, s, k, updateOp(docName, map[string]any{"title": "Should not apply"}, withExpectedVersionLegacy("2000-01-01T00:00:00Z")))
	if stale.Error == nil || stale.Error.Type != contract.CodeConflict {
		t.Fatalf("legacy expected_version must still conflict, got %+v", stale.Error)
	}
}

// TestRollbackAtomicity proves that a failure after the receipt claim rolls
// back mutation, receipt, audit, and outbox together — no partial commits.
func TestRollbackAtomicity(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	exec(t, s, k, createOp(map[string]any{"title": "Original", "serial": "SN-001"}))

	// Duplicate unique field fails inside the INSERT — after the receipt was
	// already claimed inside the same transaction.
	dup := exec(t, s, k, createOp(map[string]any{"title": "Dup", "serial": "SN-001"}, withKey("dup-key")))
	if dup.Error == nil || dup.Error.Type != contract.CodeConflict {
		t.Fatalf("unique violation must surface as CONFLICT, got %+v", dup.Error)
	}

	if s.taskCount(t) != 1 {
		t.Fatalf("failed operation must not persist the business row")
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE status = 'completed' AND result_json IS NOT NULL"); n != 0 {
		t.Fatalf("failed operation rolled back its receipt claim (found %d)", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox"); n != 1 {
		t.Fatalf("only the successful operation's outbox row may exist (found %d)", n)
	}
	auditFailures := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE status <> 'completed'")
	auditSuccess := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE status = 'completed' AND doctype = 'Task'")
	if auditSuccess != 1 {
		t.Fatalf("expected exactly 1 success audit row, got %d", auditSuccess)
	}
	_ = auditFailures
}

// TestUnknownFieldRejected proves unsupported configuration is never silently
// discarded (validation rejects unknown keys with typed errors).
func TestUnknownFieldRejected(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	bad := exec(t, s, k, createOp(map[string]any{"title": "ok", "bogus_field": "x"}))
	if bad.Error == nil || bad.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("unknown field must VALIDATION_FAILED, got %+v", bad.Error)
	}
	if s.taskCount(t) != 0 {
		t.Fatalf("invalid operation must not persist")
	}
}

// stubPublisher captures deliveries and can be made to fail.
type stubPublisher struct {
	calls atomic.Int64
	fail  atomic.Bool
	got   []contract.EventEnvelope
}

func (p *stubPublisher) Publish(_ context.Context, e contract.EventEnvelope) error {
	p.calls.Add(1)
	if p.fail.Load() {
		return fmt.Errorf("broker unavailable")
	}
	p.got = append(p.got, e)
	return nil
}

// TestOutboxDeliveryRetryAndRestart proves at-least-once delivery with retry:
// broker outage backs off without loss, recovery publishes, receipts dedup.
func TestOutboxDeliveryRetryAndRestart(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)

	res := exec(t, s, k, createOp(map[string]any{"title": "To deliver"}))
	mustComplete(t, res)

	dest := &stubPublisher{}
	dest.fail.Store(true)

	pub := outbox.NewPublisher(s.DB, dest, s.Dialect)
	pub.LeaseOwner = "test-publisher"

	// Outage window: publish attempts fail, rows stay pending.
	if _, err := pub.PublishDue(context.Background(), 10); err != nil {
		t.Fatalf("PublishDue must swallow per-row errors: %v", err)
	}
	if dest.calls.Load() == 0 {
		t.Fatalf("publisher should have attempted delivery during outage")
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE status='pending'"); n != 1 {
		t.Fatalf("event must remain pending after outage, got %d", n)
	}

	// Recovery (simulating backoff expiry after a process restart): broker
	// returns; next cycle delivers exactly once.
	dest.fail.Store(false)
	if _, err := s.DB.Exec(`UPDATE _kora_outbox SET next_attempt_at = NULL WHERE status = 'pending'`); err != nil {
		t.Fatalf("clearing backoff: %v", err)
	}
	delivered, err := pub.PublishDue(context.Background(), 10)
	if err != nil {
		t.Fatalf("recovery publish: %v", err)
	}
	if delivered != 1 || len(dest.got) != 1 {
		t.Fatalf("expected 1 delivered event, got %d", delivered)
	}
	if dest.got[0].Site != s.Name {
		t.Fatalf("delivered event carries wrong tenant: %s", dest.got[0].Site)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE status='published'"); n != 1 {
		t.Fatalf("row must be marked published after delivery")
	}

	// Restart/retry: re-running the publisher does not double-deliver.
	if _, err := pub.PublishDue(context.Background(), 10); err != nil {
		t.Fatalf("post-restart publish: %v", err)
	}
	if len(dest.got) != 1 {
		t.Fatalf("published rows must not redeliver on restart")
	}
}

// --- KERNEL-008: config-defined command resources ---

func mustRegister(t *testing.T, k *kernel.Kernel, yamlSrc string) *kernel.CommandResource {
	t.Helper()
	def, err := kernel.ParseCommandResource([]byte(yamlSrc))
	if err != nil {
		t.Fatalf("parse command: %v", err)
	}
	if err := k.Commands.Register(def); err != nil {
		t.Fatalf("register command: %v", err)
	}
	return def
}

const registerCmdYAML = `
name: animal.register
namespace: livestock
version: 1
input:
  record: Task
transaction:
  - create:
      record: Task
      values:
        title: $input.title
        serial: $input.serial
  - update:
      record: Task
      name: "$input.supervisor_ref"
      values:
        status: Done
emit:
  - animal.registered
`

// TestConfiguredCommandEndToEnd proves a YAML-defined command executes through
// the identical kernel path: atomic multi-step transaction, emitted event in
// the outbox, audit row under the command's full name.
func TestConfiguredCommandEndToEnd(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, registerCmdYAML)

	// Seed the supervisor the update step targets.
	seed := exec(t, s, k, createOp(map[string]any{"title": "Supervisor", "serial": "SUP-1"}))
	mustComplete(t, seed)

	var supName string
	s.DB.QueryRow(`SELECT name FROM ` + "`tabTask`" + ` WHERE serial='SUP-1'`).Scan(&supName)

	payload, _ := json.Marshal(map[string]any{"data": map[string]any{
		"title": "Daisy", "serial": "ANM-1", "supervisor_ref": supName,
	}})
	res := exec(t, s, k, kernel.Operation{Command: "livestock.animal.register", Payload: payload,
		Context: opCtx(s.Name, "Administrator")})
	mustComplete(t, res)

	if n := s.count(t, `SELECT COUNT(*) FROM `+"`tabTask`"+` WHERE title='Daisy'`); n != 1 {
		t.Fatalf("create step did not persist")
	}
	var status string
	s.DB.QueryRow(`SELECT status FROM ` + "`tabTask`" + ` WHERE serial='SUP-1'`).Scan(&status)
	if status != "Done" {
		t.Fatalf("update step did not apply, status=%q", status)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='animal.registered' AND site=?`, s.Name); n != 1 {
		t.Fatalf("emitted event missing from outbox")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM _kora_operation_audit WHERE command_name='livestock.animal.register' AND status='completed'`); n != 1 {
		t.Fatalf("audit row for defined command missing")
	}
}

func opCtx(site, role string) kernel.OperationContext {
	actor := contract.ActorContext{PrincipalID: "tester", PrincipalType: contract.PrincipalHuman, Site: site, Roles: []string{role}, AuthenticatedAt: time.Now()}
	execution, _ := policy.NewVerifiedExecution(contract.TenantID(site), actor)
	return kernel.OperationContext{
		Site:  site,
		User:  "tester",
		Roles: []string{role},
		Actor: actor, Execution: execution, TenantID: contract.TenantID(site),
	}
}

// TestConfiguredCommandRollbackAtomicity proves multi-step commands are all-
// or-nothing: failure in a later step discards earlier creates, receipts,
// events, and audit together.
func TestConfiguredCommandRollbackAtomicity(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, registerCmdYAML)

	payload, _ := json.Marshal(map[string]any{"data": map[string]any{
		"title": "Ghost", "serial": "GHO-1", "supervisor_ref": "TASK-MISSING",
	}})
	res := exec(t, s, k, kernel.Operation{Command: "livestock.animal.register", Payload: payload,
		Context: opCtx(s.Name, "Administrator")})
	if res.Error == nil || res.Error.Type != contract.CodeNotFound {
		t.Fatalf("missing update target must be NOT_FOUND, got %+v", res.Error)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM `+"`tabTask`"+` WHERE serial='GHO-1'`); n != 0 {
		t.Fatalf("earlier create survived step failure — rollback broken")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM _kora_operation_audit WHERE command_name='livestock.animal.register' AND status='completed'`); n != 0 {
		t.Fatalf("failed command must not leave success audit")
	}
}

func TestConfiguredCommandRejectsStaleVersionAtomically(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, registerCmdYAML)

	seed := exec(t, s, k, createOp(map[string]any{"title": "Supervisor", "serial": "SUP-STALE"}))
	mustComplete(t, seed)
	var supervisor string
	if err := s.DB.QueryRow(`SELECT name FROM ` + "`tabTask`" + ` WHERE serial='SUP-STALE'`).Scan(&supervisor); err != nil {
		t.Fatal("load supervisor record:", err)
	}
	payload, _ := json.Marshal(map[string]any{"data": map[string]any{
		"title": "Must roll back", "serial": "STALE-1", "supervisor_ref": supervisor,
	}})
	ctx := opCtx(s.Name, "Administrator")
	ctx.ExpectedVersion = "2000-01-01T00:00:00.000000Z"
	result := exec(t, s, k, kernel.Operation{Command: "livestock.animal.register", Payload: payload, Context: ctx})
	if result.Error == nil || result.Error.Type != contract.CodeConflict {
		t.Fatalf("stale command update = %+v, want conflict", result.Error)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM `+"`tabTask`"+` WHERE serial='STALE-1'`); count != 0 {
		t.Fatalf("stale command retained its earlier create step: count=%d", count)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='animal.registered' AND site=?`, s.Name); count != 0 {
		t.Fatalf("stale command emitted success event: count=%d", count)
	}
}

// TestConfiguredCommandAuthzDenied proves authorization evaluates every
// touched record through the same matrix — no adapter can slip past.
func TestConfiguredCommandAuthzDenied(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	mustRegister(t, k, registerCmdYAML)

	payload, _ := json.Marshal(map[string]any{"data": map[string]any{
		"title": "X", "serial": "X-1", "supervisor_ref": "whatever",
	}})
	ctx := opCtx(s.Name, "Creator")
	res := exec(t, s, k, kernel.Operation{Command: "livestock.animal.register", Payload: payload, Context: ctx})
	if res.Error == nil || res.Error.Type != contract.CodePermissionDenied {
		t.Fatalf("Creator lacks write on Task; expected PERMISSION_DENIED, got %+v", res.Error)
	}
	if s.taskCount(t) != 0 {
		t.Fatalf("denied command wrote data")
	}
}

type trustedOwnerCommandSchemas struct {
	input, output contract.SchemaRef
	rejectInput   bool
	rejectOutput  bool
}

func (s *trustedOwnerCommandSchemas) Registered(ref contract.SchemaRef) bool {
	return ref == s.input || ref == s.output
}

func (s *trustedOwnerCommandSchemas) Validate(ref contract.SchemaRef, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("invalid JSON")
	}
	if ref == s.input && s.rejectInput {
		return fmt.Errorf("injected input rejection")
	}
	if ref == s.output && s.rejectOutput {
		return fmt.Errorf("injected output rejection")
	}
	return nil
}

func TestTrustedOwnerCommandValidatesBeforeBusinessEffects(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	invoice := &doctype.DocType{
		Name: "Invoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands),
		BusinessScopePolicy: contract.ScopeTenantShared,
		Fields:              []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}},
	}
	s.Registry.Register(invoice)
	s.Registry.Permissions.SetPermission(&doctype.Permission{Doctype: invoice.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true})
	if err := schema.MigrateSiteFromRegistry(s.DB, s.Name, s.Registry, s.Dialect); err != nil {
		t.Fatal("migrate protected invoice resource:", err)
	}

	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()
	command, err := kernel.ParseCommandResource([]byte(`
name: issue
namespace: finance.invoice
version: 1
input:
  record: Invoice
module_owner_id: finance.ar
capability: finance.invoice.issue
operation: issue
input_schema:
  id: finance.invoice.issue.input
  version: 1
output_schema:
  id: finance.invoice.issue.output
  version: 1
transaction:
  - create:
      record: Invoice
      values:
        title: $input.title
business_events:
  - type: finance.invoice.issued
    owner_module_id: finance.ar
    payload_schema:
      id: finance.invoice.issue.input
      version: 1
`))
	if err != nil {
		t.Fatal("parse owner command:", err)
	}
	if err := k.Commands.Register(command); err != nil {
		t.Fatal("register owner command:", err)
	}
	schemas := &trustedOwnerCommandSchemas{input: command.InputSchema, output: command.OutputSchema}
	k.CommandSchemas = schemas
	scope := contract.BusinessScope{TenantID: contract.TenantID(s.Name), Kind: contract.ScopeKindTenant}
	moduleExecution, err := contract.NewCommandModuleExecution(scope, "finance.ar", "finance.ar", 1, command.FullName(), command.FullName())
	if err != nil {
		t.Fatal("mint internal owner command execution:", err)
	}
	context := opCtx(s.Name, doctype.AdminRole)
	context.BusinessScope = scope
	context.ModuleExecution = &moduleExecution

	invalidPayload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": ""}})
	invalid := exec(t, s, k, kernel.Operation{Command: command.FullName(), Payload: invalidPayload, Context: context})
	if invalid.Error == nil || invalid.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("owner command with invalid domain data = %+v, want validation failure", invalid.Error)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM `+"`tabInvoice`"); count != 0 {
		t.Fatalf("invalid owner command wrote %d invoices", count)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='finance.invoice.issued' AND site=?`, s.Name); count != 0 {
		t.Fatalf("invalid owner command emitted %d success events", count)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM _kora_operation_audit WHERE command_name=? AND status='completed'`, command.FullName()); count != 0 {
		t.Fatalf("invalid owner command wrote %d success audit rows", count)
	}

	bundlePayload := kernel.RecordMutationBundlePayload{Doctype: "Task", Records: []kernel.RecordMutationBundleItem{
		{Key: "task", Doctype: "Task", Data: json.RawMessage(`{"title":"must roll back before protected invoice"}`)},
		{Key: "invoice", Doctype: invoice.Name, Data: json.RawMessage(`{"title":"generic bundle bypass"}`)},
	}}
	bundle := bundleOp(bundlePayload, withRoles(doctype.AdminRole), withKey("protected-module-bundle"))
	beforeTasks := s.taskCount(t)
	beforeInvoices := s.count(t, `SELECT COUNT(*) FROM `+"`tabInvoice`")
	beforeEvents := s.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE site=?`, s.Name)
	beforeCompleted := s.count(t, `SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name=? AND status='completed'`, s.Name, kernel.CommandRecordMutateBundle)
	beforeReceipts := s.count(t, `SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name=?`, s.Name, kernel.CommandRecordMutateBundle)
	deniedBundle := exec(t, s, k, bundle)
	if deniedBundle.Error == nil || deniedBundle.Error.Type != contract.CodePermissionDenied {
		t.Fatalf("generic bundle touching an owner resource = %+v, want permission denied", deniedBundle.Error)
	}
	if got := s.taskCount(t); got != beforeTasks {
		t.Fatalf("denied protected-resource bundle changed task rows from %d to %d", beforeTasks, got)
	}
	if got := s.count(t, `SELECT COUNT(*) FROM `+"`tabInvoice`"); got != beforeInvoices {
		t.Fatalf("denied protected-resource bundle changed invoice rows from %d to %d", beforeInvoices, got)
	}
	if got := s.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE site=?`, s.Name); got != beforeEvents {
		t.Fatalf("denied protected-resource bundle changed outbox rows from %d to %d", beforeEvents, got)
	}
	if got := s.count(t, `SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name=? AND status='completed'`, s.Name, kernel.CommandRecordMutateBundle); got != beforeCompleted {
		t.Fatalf("denied protected-resource bundle wrote %d success audits", got-beforeCompleted)
	}
	if got := s.count(t, `SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name=?`, s.Name, kernel.CommandRecordMutateBundle); got != beforeReceipts {
		t.Fatalf("denied protected-resource bundle wrote %d idempotency receipts", got-beforeReceipts)
	}

	validPayload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": "INV-1"}})
	schemas.rejectInput = true
	invalidSchema := exec(t, s, k, kernel.Operation{Command: command.FullName(), Payload: validPayload, Context: context})
	if invalidSchema.Error == nil || invalidSchema.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("owner command with input schema mismatch = %+v, want validation failure", invalidSchema.Error)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM `+"`tabInvoice`"); count != 0 {
		t.Fatalf("input schema rejection wrote %d invoices", count)
	}
	schemas.rejectInput = false
	schemas.rejectOutput = true
	invalidOutput := exec(t, s, k, kernel.Operation{Command: command.FullName(), Payload: validPayload, Context: context})
	if invalidOutput.Error == nil || invalidOutput.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("owner command with output schema mismatch = %+v, want validation failure", invalidOutput.Error)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM `+"`tabInvoice`"); count != 0 {
		t.Fatalf("output schema rejection left %d invoices", count)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='finance.invoice.issued' AND site=?`, s.Name); count != 0 {
		t.Fatalf("output schema rejection emitted %d success events", count)
	}
	schemas.rejectOutput = false
	valid := exec(t, s, k, kernel.Operation{Command: command.FullName(), Payload: validPayload, Context: context})
	mustComplete(t, valid)
	if count := s.count(t, `SELECT COUNT(*) FROM `+"`tabInvoice`"+` WHERE title='INV-1'`); count != 1 {
		t.Fatalf("trusted owner command persisted %d invoices, want one", count)
	}
	if count := s.count(t, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='finance.invoice.issued' AND site=?`, s.Name); count != 1 {
		t.Fatalf("valid owner command emitted %d events, want one", count)
	}
}

// TestConfiguredCommandIdempotentReplay proves defined commands honor
// idempotency keys identically to built-in commands.
func TestConfiguredCommandIdempotentReplay(t *testing.T) {
	s := newSite(t, "site-a")
	defer s.DB.Close()
	k := newKernel(s)
	k.Commands = kernel.NewCommandRegistry()

	oneStepYAML := `
name: solo.create
namespace: livestock
version: 1
input:
  record: Task
transaction:
  - create:
      record: Task
      values:
        title: $input.title
`
	mustRegister(t, k, oneStepYAML)

	payload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": "Once"}})
	op := kernel.Operation{Command: "livestock.solo.create", Payload: payload,
		Context: opCtx(s.Name, "Administrator")}
	op.Context.IdempotencyKey = "cmd-key-1"

	first := exec(t, s, k, op)
	mustComplete(t, first)
	second := exec(t, s, k, op)
	mustComplete(t, second)
	if !second.Replayed {
		t.Fatalf("defined command replay must be flagged")
	}
	changedPayload, _ := json.Marshal(map[string]any{"data": map[string]any{"title": "Different"}})
	conflict := exec(t, s, k, kernel.Operation{Command: op.Command, Payload: changedPayload, Context: op.Context})
	if conflict.Error == nil || conflict.Error.Type != contract.CodeIdempotencyKeyReused {
		t.Fatalf("configured command key reuse with another payload = %+v, want IDEMPOTENCY_KEY_REUSED", conflict.Error)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM `+"`tabTask`"+` WHERE title='Once'`); n != 1 {
		t.Fatalf("replay duplicated the mutation")
	}
}
