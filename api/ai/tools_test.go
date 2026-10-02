package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

func TestBuildToolCatalogAllowsWhatsAppWithGuards(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name: "Customer",
		Fields: []doctype.Field{
			{Fieldname: "name", Fieldtype: "Data", Label: "Name", Reqd: true},
			{Fieldname: "email", Fieldtype: "Data", Label: "Email"},
		},
	})

	catalog := BuildToolCatalog(reg)
	create := findTool(t, catalog.Tools, "customer_create")
	if !allowsChannel(create.ChannelAllowlist, "whatsapp") {
		t.Fatalf("customer_create should be allowed on whatsapp: %#v", create.ChannelAllowlist)
	}
	if !create.RequiresConfirmation || !create.RequiresRecentAuth {
		t.Fatalf("customer_create should remain guarded: %#v", create)
	}
	update := findTool(t, catalog.Tools, "customer_update")
	if !allowsChannel(update.ChannelAllowlist, "whatsapp") {
		t.Fatalf("customer_update should be allowed on whatsapp: %#v", update.ChannelAllowlist)
	}
	if !update.RequiresConfirmation || !update.RequiresRecentAuth || update.Operation != "update" || update.Doctype != "Customer" {
		t.Fatalf("customer_update should be guarded update metadata: %#v", update)
	}
	props := update.InputSchema["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Fatalf("customer_update should require stable document name: %#v", update.InputSchema)
	}
	required := update.InputSchema["required"].([]string)
	if len(required) != 1 || required[0] != "name" {
		t.Fatalf("customer_update should only require name: %#v", update.InputSchema)
	}

	system := findTool(t, catalog.Tools, "script_create")
	if !allowsChannel(system.ChannelAllowlist, "whatsapp") {
		t.Fatalf("script_create should be allowed on whatsapp for beta parity")
	}
	if !system.RequiresConfirmation || !system.RequiresRecentAuth {
		t.Fatalf("script_create should remain guarded: %#v", system)
	}
}

func TestExecuteSingleToolDeniesDocTypeOperationWithoutRolePermission(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "PurchaseRequest"})
	tx := &orm.TxManager{Context: context.Background()}
	result := executeSingleToolWithRolesAndCallID(tx, reg, "purchaserequest_list", map[string]any{}, "agent", "site", "run", "step", "", []string{"Guest"})
	if !strings.Contains(result, "Permission denied") {
		t.Fatalf("expected permission denial, got %q", result)
	}
}

func TestUpdateToolAllowsStableNameArgument(t *testing.T) {
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
			{Fieldname: "status", Fieldtype: "Select", Label: "Status", Options: "Todo\nDone"},
		},
	}

	unknown := unknownFieldsExcept(map[string]any{
		"name":   "TASK-0001",
		"status": "Done",
	}, dt, map[string]bool{"name": true})
	if len(unknown) != 0 {
		t.Fatalf("expected update name argument to be allowed, got %#v", unknown)
	}
	unknown = unknownFieldsExcept(map[string]any{
		"name":    "TASK-0001",
		"missing": "value",
	}, dt, map[string]bool{"name": true})
	if len(unknown) != 1 || unknown[0] != "missing" {
		t.Fatalf("expected only real unknown fields to be rejected, got %#v", unknown)
	}
}

func TestExecuteUpdateToolRejectsInvalidArgumentsBeforeMutation(t *testing.T) {
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
			{Fieldname: "calculated", Fieldtype: "Data", Label: "Calculated", ReadOnly: true},
		},
	}
	reg := doctype.NewRegistry()
	reg.Register(dt)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "missing record name", args: map[string]any{"title": "Updated"}, want: "name is required"},
		{name: "unknown field", args: map[string]any{"name": "TASK-1", "missing": "value"}, want: "unknown fields: missing"},
		{name: "read-only field", args: map[string]any{"name": "TASK-1", "calculated": "forged"}, want: "field calculated is not writable"},
		{name: "empty change set", args: map[string]any{"name": "TASK-1"}, want: "provide at least one writable field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := executeUpdateToolWithKey(nil, reg, dt, test.args, "alice@example.test", []string{doctype.AdminRole}, "", false)
			if !strings.Contains(got, test.want) {
				t.Fatalf("update result = %q, want it to contain %q", got, test.want)
			}
		})
	}
}

func TestAIToolMutationIdempotencyKeyIsStableAndScoped(t *testing.T) {
	key := aiToolMutationIdempotencyKey("site-a", "run-1", "step-1", "call-1")
	if key == "" || key != aiToolMutationIdempotencyKey("site-a", "run-1", "step-1", "call-1") {
		t.Fatalf("same tool invocation must produce a stable non-empty idempotency key: %q", key)
	}
	if key == aiToolMutationIdempotencyKey("site-a", "run-1", "step-1", "call-2") || key == aiToolMutationIdempotencyKey("site-b", "run-1", "step-1", "call-1") {
		t.Fatal("different tool invocations or sites must not share an idempotency key")
	}
	if got := aiToolMutationIdempotencyKey("site-a", "run-1", "", "call-1"); got != "" {
		t.Fatalf("incomplete invocation identity must not enable accidental cross-call deduplication: %q", got)
	}
}

func TestRolesForToolUsesAuthenticatedTxRoles(t *testing.T) {
	tx := &orm.TxManager{CurrentUserRole: "Cashier", CurrentUserRoles: []string{"Cashier", "POS Manager"}}
	got := rolesForTool(tx)
	if len(got) != 2 || got[0] != "Cashier" || got[1] != "POS Manager" {
		t.Fatalf("rolesForTool = %#v, want both resolved roles", got)
	}
	got[0] = "mutated"
	if tx.CurrentUserRoles[0] != "Cashier" {
		t.Fatal("rolesForTool exposed the mutable request role slice")
	}
	if got := rolesForTool(&orm.TxManager{CurrentUserRole: "Cashier"}); len(got) != 1 || got[0] != "Cashier" {
		t.Fatalf("primary-role fallback = %#v, want Cashier", got)
	}
}

func TestChannelToolIdempotencyKeyIsBoundedAndSiteScoped(t *testing.T) {
	key := channelToolMutationIdempotencyKey("site-a", strings.Repeat("client-key", 100))
	if len(key) > 100 || key != channelToolMutationIdempotencyKey("site-a", strings.Repeat("client-key", 100)) {
		t.Fatalf("channel key must be deterministic and bounded, got length %d", len(key))
	}
	if key == channelToolMutationIdempotencyKey("site-b", strings.Repeat("client-key", 100)) {
		t.Fatal("same client key in different sites must be distinct at the adapter")
	}
	if channelToolMutationIdempotencyKey("site-a", " ") != "" {
		t.Fatal("blank client key must not enable implicit deduplication")
	}
}

func TestBuildToolCatalogEmitsV2FindSchemaAndMetadata(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name:         "Task",
		TitleField:   "title",
		SearchFields: "title, status",
		SortField:    "modified",
		SortOrder:    "DESC",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title", Reqd: true, SearchIndex: true},
			{Fieldname: "due_date", Fieldtype: "Date", Label: "Due Date", InStandardFilter: true},
			{Fieldname: "status", Fieldtype: "Select", Label: "Status", Options: "Todo\nDone"},
		},
	})

	catalog := BuildToolCatalog(reg)
	find := findTool(t, catalog.Tools, "task_find")
	if find.ArgumentContractVersion != "v2" || find.Operation != "find" || find.Doctype != "Task" {
		t.Fatalf("expected v2 find metadata, got %#v", find)
	}
	props := find.InputSchema["properties"].(map[string]any)
	if _, ok := props["filters"]; !ok {
		t.Fatalf("expected filters property in find schema: %#v", props)
	}
	if _, ok := props["due_date"]; ok {
		t.Fatalf("find schema should not expose flat field args: %#v", props)
	}
	if len(find.FieldHints) != 3 || len(find.SystemFields) == 0 {
		t.Fatalf("expected field and system hints, got %#v", find)
	}
}

func TestListFormattingUsesSchemaSummary(t *testing.T) {
	dt := &doctype.DocType{
		Name:       "Task",
		TitleField: "title",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title", InListView: true},
			{Fieldname: "due_date", Fieldtype: "Date", Label: "Due Date", InListView: true},
			{Fieldname: "priority", Fieldtype: "Select", Label: "Priority", InListView: true, Options: "Low\nMedium\nHigh"},
			{Fieldname: "creation", Fieldtype: "Datetime", Label: "Creation"},
		},
	}
	doc := doctype.NewDocument("Task")
	doc.Name = "TASK-0001"
	doc.Fields = map[string]any{
		"title":    "Call home",
		"due_date": "2026-07-14 00:00:00 +0000 UTC",
		"priority": "Medium",
		"creation": "2026-07-13 20:49:53 +0000 UTC",
	}
	out := formatDocSummary(dt, doc, 1)

	if strings.Contains(out, "map[") || strings.Contains(out, "+0000 UTC") || strings.Contains(out, "Creation") {
		t.Fatalf("expected clean schema summary, got %q", out)
	}
	for _, want := range []string{"1. Call home", "ID: TASK-0001", "Due Date: 2026-07-14", "Priority: Medium"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in summary %q", want, out)
		}
	}
}

func TestBuildValidatedFindArgsRejectsInvalidFindInputs(t *testing.T) {
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
			{Fieldname: "due_date", Fieldtype: "Date", Label: "Due Date"},
			{Fieldname: "priority", Fieldtype: "Int", Label: "Priority"},
		},
	}

	if _, _, _, _, err := buildValidatedFindArgs(dt, map[string]any{"due_date": "2026-07-15"}); err == nil {
		t.Fatal("expected flat field args to be rejected")
	}
	if _, _, _, _, err := buildValidatedFindArgs(dt, map[string]any{
		"filters": []any{map[string]any{"field": "missing", "op": "=", "value": "x"}},
	}); err == nil {
		t.Fatal("expected unknown filter field to be rejected")
	}
	if _, _, _, _, err := buildValidatedFindArgs(dt, map[string]any{
		"filters": []any{map[string]any{"field": "priority", "op": "like", "value": "1"}},
	}); err == nil {
		t.Fatal("expected invalid numeric operator to be rejected")
	}
	if _, _, _, _, err := buildValidatedFindArgs(dt, map[string]any{
		"filters":  []any{map[string]any{"field": "due_date", "op": "=", "value": "2026-07-15"}},
		"order_by": "missing DESC",
	}); err == nil {
		t.Fatal("expected invalid order_by to be rejected")
	}
}

func TestBuildValidatedFindArgsBuildsTypedORMFilters(t *testing.T) {
	dt := &doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "due_date", Fieldtype: "Date", Label: "Due Date"},
			{Fieldname: "priority", Fieldtype: "Int", Label: "Priority"},
		},
	}

	filter, limit, offset, orderBy, err := buildValidatedFindArgs(dt, map[string]any{
		"filters": []any{
			map[string]any{"field": "due_date", "op": "=", "value": "2026-07-15"},
			map[string]any{"field": "priority", "op": ">=", "value": "2"},
		},
		"limit":    float64(7),
		"offset":   float64(3),
		"order_by": "modified DESC",
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed [][]any
	if err := json.Unmarshal([]byte(filter), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || parsed[0][0] != "due_date" || parsed[0][2] != "2026-07-15" || parsed[1][0] != "priority" || parsed[1][1] != ">=" || parsed[1][2] != float64(2) || limit != 7 || offset != 3 || orderBy != "modified DESC" {
		t.Fatalf("unexpected find args: filter=%s limit=%d offset=%d order=%q", filter, limit, offset, orderBy)
	}
}

func TestRequireRecentAuthForTool(t *testing.T) {
	ctx := context.WithValue(context.Background(), "session_created_at", time.Now().Add(-5*time.Minute))
	if err := requireRecentAuthForTool(ctx, "create_doctype_draft"); err != nil {
		t.Fatalf("expected recent auth to pass, got %v", err)
	}

	oldCtx := context.WithValue(context.Background(), "session_created_at", time.Now().Add(-15*time.Minute))
	if err := requireRecentAuthForTool(oldCtx, "create_doctype_draft"); err == nil {
		t.Fatal("expected stale auth to be rejected")
	}

	if err := requireRecentAuthForTool(context.Background(), "list_doctypes"); err != nil {
		t.Fatalf("non-guarded tool should not require recent auth, got %v", err)
	}
}

func TestExecuteSingleToolScriptCreateRequiresApproval(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(1\)[\s\S]*FROM _kora_ai_approval[\s\S]*state = 'granted'[\s\S]*target_fingerprint = \?`).
		WithArgs("site-a", "run-1", "script_create", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_ai_approval`).
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	tx := &orm.TxManager{DB: db}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now())
	got := executeSingleTool(tx, doctype.NewRegistry(), "script_create", map[string]any{
		"name":        "hello_script",
		"script_type": "validate",
		"script":      "return true;",
	}, "alice", "site-a", "run-1", "")
	if !strings.Contains(got, "Approval required for script_create") {
		t.Fatalf("expected approval gate, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestExecuteSingleToolScriptCreateRejectsStaleAuth(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tx := &orm.TxManager{DB: db}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now().Add(-20*time.Minute))
	got := executeSingleTool(tx, doctype.NewRegistry(), "script_create", map[string]any{
		"name":        "hello_script",
		"script_type": "validate",
		"script":      "return true;",
	}, "alice", "site-a", "run-1", "step-1")
	if !strings.Contains(got, "recent authentication required") {
		t.Fatalf("expected recent auth failure, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected sql queries: %v", err)
	}
}

func TestExecuteSingleToolUpdateDoctypeDraftRequiresApproval(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	mock.ExpectQuery(`SELECT COUNT\(1\)[\s\S]*FROM _kora_ai_approval[\s\S]*state = 'granted'[\s\S]*target_fingerprint = \?`).
		WithArgs("site-a", "run-1", "update_doctype_draft", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_ai_approval`).
		WithArgs(
			sqlmock.AnyArg(), "site-a", "run-1", "alice", "agent",
			"update_doctype_draft", "pending_approval", sqlmock.AnyArg(), sqlmock.AnyArg(), 0,
			sqlmock.AnyArg(), nil, nil, "", "",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	tx := &orm.TxManager{DB: database}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now())
	got := executeSingleTool(tx, doctype.NewRegistry(), "update_doctype_draft", map[string]any{
		"yaml": "name: Task\nfields:\n  - fieldname: title\n    fieldtype: Data\n",
	}, "alice", "site-a", "run-1", "")
	if !strings.Contains(got, "Approval required for update_doctype_draft") {
		t.Fatalf("expected approval gate, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestExecuteSingleToolUpdateDoctypeDraftRejectsStaleAuth(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	tx := &orm.TxManager{DB: database}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now().Add(-20*time.Minute))
	got := executeSingleTool(tx, doctype.NewRegistry(), "update_doctype_draft", map[string]any{
		"yaml": "name: Task\nfields:\n  - fieldname: title\n    fieldtype: Data\n",
	}, "alice", "site-a", "run-1", "step-1")
	if !strings.Contains(got, "recent authentication required") {
		t.Fatalf("expected recent auth failure, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected sql queries: %v", err)
	}
}

func TestExecuteSingleToolUpdateDoctypeDraftExecutesAfterApproval(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name: "Task",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
		},
	})

	mock.ExpectQuery(`SELECT COUNT\(1\)[\s\S]*FROM _kora_ai_approval[\s\S]*state = 'granted'[\s\S]*target_fingerprint = \?`).
		WithArgs("site-a", "run-1", "update_doctype_draft", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT id, config FROM _kora_config_version[\s\S]*status IN \('Draft', 'Active'\)[\s\S]*LIMIT 1`).
		WithArgs("site-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "config"}).
			AddRow("cv-base-1", `{"doctypes":[]}`))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM _kora_config_version WHERE site = \?`).
		WithArgs("site-a").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(0))
	mock.ExpectQuery(`SELECT config FROM _kora_config_version WHERE site = \? AND version = \?`).
		WithArgs("site-a", 0).
		WillReturnRows(sqlmock.NewRows([]string{"config"}))
	mock.ExpectExec(`INSERT INTO _kora_config_version`).
		WithArgs(
			sqlmock.AnyArg(), "site-a", 1, "alice", "Updated Task via AI (Draft)",
			sqlmock.AnyArg(), "Draft", false, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"cv-base-1", "",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT id[\s\S]*FROM _kora_ai_approval[\s\S]*state = 'pending_approval'[\s\S]*target_fingerprint = \?[\s\S]*LIMIT 1`).
		WithArgs("site-a", "run-1", "update_doctype_draft", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	tx := &orm.TxManager{DB: database, Dialect: db.Resolve("mysql")}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now())
	got := executeSingleTool(tx, reg, "update_doctype_draft", map[string]any{
		"yaml": "name: Task\nmodule: Core\ntitle_field: title\nfields:\n  - fieldname: title\n    fieldtype: Data\n    label: Title\n    reqd: true\n",
	}, "alice", "site-a", "run-1", "")
	if !strings.Contains(got, `Updated DocType "Task" as DRAFT`) {
		t.Fatalf("expected draft update result, got %q", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestExecuteToolCallsForAIRecordsAuditRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ctx := enrichAuditContext(context.Background(), "user-1", "sid-1", "corr-1", "idem-1")
	mock.ExpectExec(`INSERT INTO _kora_ai_audit`).
		WithArgs(
			sqlmock.AnyArg(),
			"site-a",
			"run-1",
			"step-1",
			"conv-1",
			"tool_call",
			"list_doctypes",
			"completed",
			"user-1",
			"sid-1",
			"corr-1",
			"idem-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	tx := &orm.TxManager{DB: db}
	results := executeToolCallsForAI(ctx, tx, doctype.NewRegistry(), []any{
		map[string]any{
			"id": "tool-call-1",
			"function": map[string]any{
				"name":      "list_doctypes",
				"arguments": "{}",
			},
		},
	}, "alice", "site-a", "run-1", "step-1", "conv-1", nil)

	if len(results) != 1 {
		t.Fatalf("expected one tool result, got %d", len(results))
	}
	if results[0]["tool_call_id"] != "tool-call-1" {
		t.Fatalf("unexpected tool_call_id: %#v", results[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestExecuteToolCallsForAIGuardedToolUsesSharedExecutor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ctx := enrichAuditContext(
		context.WithValue(context.Background(), "session_created_at", time.Now()),
		"user-1",
		"sid-1",
		"corr-1",
		"idem-1",
	)

	mock.ExpectQuery(`SELECT COUNT\(1\)[\s\S]*FROM _kora_ai_approval[\s\S]*state = 'granted'[\s\S]*target_fingerprint = \?`).
		WithArgs("site-a", "run-1", "script_create", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_ai_approval`).
		WithArgs(
			sqlmock.AnyArg(),
			"site-a",
			"run-1",
			"alice",
			"agent",
			"script_create",
			"pending_approval",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			0,
			sqlmock.AnyArg(),
			nil,
			nil,
			"",
			"",
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO _kora_ai_audit`).
		WithArgs(
			sqlmock.AnyArg(),
			"site-a",
			"run-1",
			"step-1",
			"conv-1",
			"tool_call",
			"script_create",
			"completed",
			"user-1",
			"sid-1",
			"corr-1",
			"idem-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	tx := &orm.TxManager{DB: db}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now())
	results := executeToolCallsForAI(ctx, tx, doctype.NewRegistry(), []any{
		map[string]any{
			"id": "tool-call-guarded-1",
			"function": map[string]any{
				"name":      "script_create",
				"arguments": `{"name":"hello_script","script_type":"validate","script":"return true;"}`,
			},
		},
	}, "alice", "site-a", "run-1", "step-1", "conv-1", nil)

	if len(results) != 1 {
		t.Fatalf("expected one tool result, got %d", len(results))
	}
	if results[0]["tool_call_id"] != "tool-call-guarded-1" {
		t.Fatalf("unexpected tool_call_id: %#v", results[0])
	}
	content, _ := results[0]["content"].(string)
	if !strings.Contains(content, "Approval required for script_create") {
		t.Fatalf("expected approval gate content, got %q", content)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func findTool(t *testing.T, tools []ToolDescriptor, name string) ToolDescriptor {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return ToolDescriptor{}
}

func allowsChannel(values []string, channel string) bool {
	for _, value := range values {
		if value == channel {
			return true
		}
	}
	return false
}
