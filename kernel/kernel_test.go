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
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/script"
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
	DB       *sql.DB
	Name     string
	Registry *doctype.Registry
	Dialect  kdb.Dialect
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
		},
	}
	roles := []*doctype.Role{{Name: "Administrator"}, {Name: "Creator"}, {Name: "OwnerEditor"}}
	perms := []*doctype.Permission{
		{Doctype: "Task", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true, Submit: true},
		{Doctype: "Task", Role: "Creator", Read: true, Create: true}, // create-only: no write
		{Doctype: "Task", Role: "OwnerEditor", Read: true, Write: true, Create: true, IfOwner: true},
	}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{taskDT}, roles, perms)
	if err := schema.MigrateSiteFromRegistry(sdb, dbName, reg, dialect); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &siteFixture{DB: sdb, Name: name, Registry: reg, Dialect: dialect}
}

func newKernel(s *siteFixture) *kernel.Kernel {
	return kernel.New(s.Dialect, outbox.NewSQLWriter(s.Dialect))
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
		op.Context.Actor = contract.ActorContext{
			PrincipalID:     user,
			PrincipalType:   contract.PrincipalHuman,
			Site:            s.Name,
			Roles:           op.Context.Roles,
			AuthenticatedAt: time.Now(),
		}
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
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", s.Name, "idem-1"); n != 1 {
		t.Fatalf("expected 1 receipt, got %d", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", s.Name, docName); n != 1 {
		t.Fatalf("expected 1 audit row, got %d", n)
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
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND idempotency_key=?", s.Name, "concurrent-create-key"); got != 1 {
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
	if got := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND idempotency_key=?", s.Name, "concurrent-bundle-retry"); got != 1 {
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
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", s.Name, "delete-once"); n != 1 {
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
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = ?", s.Name, "bundle-create-update"); n != 1 {
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
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ? AND idempotency_key = 'bundle-rollback'", s.Name); n != 0 {
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
	for index := 0; index < 11; index++ {
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

func TestRecordWorkflowTransitionUsesCanonicalPipeline(t *testing.T) {
	s := newSite(t, "site-workflow")
	defer s.DB.Close()
	s.Registry.Workflows.Register(&doctype.Workflow{
		Name: "Task Lifecycle", DocumentType: "Task", IsActive: true, WorkflowStateField: "status",
		States: []doctype.WorkflowState{
			{State: "Open", DocStatus: 0},
			{State: "Done", DocStatus: 1},
		},
		Transitions: []doctype.WorkflowTransition{{Action: "Complete", From: "Open", To: "Done", Allowed: "Administrator", RequireFields: []string{"title"}}},
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
	op := kernel.Operation{Command: kernel.CommandRecordWorkflowTransition, Payload: payload}
	op.Context.Site = s.Name
	op.Context.User = "Administrator"
	op.Context.Roles = []string{"Administrator"}
	op.Context.Actor = contract.ActorContext{PrincipalID: "Administrator", PrincipalType: contract.PrincipalHuman, Site: s.Name, Roles: op.Context.Roles, AuthenticatedAt: time.Now()}
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
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND command_name = ?", s.Name, docName, kernel.CommandRecordWorkflowTransition); n != 1 {
		t.Fatalf("workflow audit rows = %d, want 1", n)
	}
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", s.Name, docName); n != 2 {
		t.Fatalf("create + transition outbox rows = %d, want 2", n)
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

	sources := []kernel.Source{kernel.SourceHTTP, kernel.SourceSDK, kernel.SourceMCP, kernel.SourceAI, kernel.SourceCLI}
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
	}
	if s.taskCount(t) != len(sources) {
		t.Fatalf("parity drift: expected one doc per allowed source execution")
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
	if n := s.count(t, "SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE idempotency_key = 'dup-key'"); n != 0 {
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
	return kernel.OperationContext{
		Site:  site,
		User:  "tester",
		Roles: []string{role},
		Actor: contract.ActorContext{PrincipalID: "tester", PrincipalType: contract.PrincipalHuman, Site: site, AuthenticatedAt: time.Now()},
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
	if n := s.count(t, `SELECT COUNT(*) FROM `+"`tabTask`"+` WHERE title='Once'`); n != 1 {
		t.Fatalf("replay duplicated the mutation")
	}
}
