package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
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
	if create.RequiresConfirmation || create.RequiresRecentAuth {
		t.Fatalf("customer_create should use the user's explicit request and ordinary role policy: %#v", create)
	}
	update := findTool(t, catalog.Tools, "customer_update")
	if !allowsChannel(update.ChannelAllowlist, "whatsapp") {
		t.Fatalf("customer_update should be allowed on whatsapp: %#v", update.ChannelAllowlist)
	}
	if update.RequiresConfirmation || update.RequiresRecentAuth || update.Operation != "update" || update.Doctype != "Customer" {
		t.Fatalf("customer_update should use explicit user intent with kernel authorization: %#v", update)
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
	if system.RequiresConfirmation || !system.RequiresRecentAuth {
		t.Fatalf("script_create should remain a recent-authenticated draft action: %#v", system)
	}
}

func TestMutationBundleToolRequiresConfirmationAndRevisionedUpdates(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Lead", Fields: []doctype.Field{
		{Fieldname: "lead_name", Fieldtype: "Data", Reqd: true},
		{Fieldname: "status", Fieldtype: "Select", Options: "New\nQualified"},
		{Fieldname: "converted_deal", Fieldtype: "Link", Options: "Deal"},
		{Fieldname: "private_note", Fieldtype: "Data", Hidden: true},
	}})
	reg.Register(&doctype.DocType{Name: "Company", Fields: []doctype.Field{{Fieldname: "company_name", Fieldtype: "Data", Reqd: true}}})
	reg.Register(&doctype.DocType{Name: "Deal", Fields: []doctype.Field{
		{Fieldname: "deal_name", Fieldtype: "Data", Reqd: true},
		{Fieldname: "lead", Fieldtype: "Link", Options: "Lead"},
		{Fieldname: "items", Fieldtype: "Table", Options: "Deal Item", Reqd: true},
	}})
	reg.Register(&doctype.DocType{Name: "Deal Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product_name", Fieldtype: "Data", Reqd: true},
		{Fieldname: "unit_price", Fieldtype: "Currency", Reqd: true},
		{Fieldname: "line_total", Fieldtype: "Currency", Computed: "quantity * unit_price", ReadOnly: true},
	}})
	catalog := BuildToolCatalog(reg)
	tool := findTool(t, catalog.Tools, "mutate_records")
	if !tool.RequiresConfirmation || tool.RequiresRecentAuth || tool.Operation != "system" || allowsChannel(tool.ChannelAllowlist, "whatsapp") {
		t.Fatalf("mutate_records must be a confirmed generic operation: %#v", tool)
	}
	if !strings.Contains(tool.Description, "atomic") || !strings.Contains(tool.Description, "approval") || !strings.Contains(tool.Description, "Deal.items") || !strings.Contains(tool.Description, "product_name (required)") {
		t.Fatalf("mutate_records description lacks transaction/approval guidance: %q", tool.Description)
	}

	args := map[string]any{"records": []any{
		map[string]any{"key": "deal", "doctype": "Deal", "data": map[string]any{"deal_name": "Northwind renewal"}},
		map[string]any{"key": "lead", "doctype": "Lead", "operation": "update", "name": "LEAD-0001", "expected_revision": float64(7), "transition_action": "Convert", "data": map[string]any{"converted_deal": "$records.deal.name"}},
	}}
	payload, err := buildAIMutationBundle(reg, args)
	if err != nil {
		t.Fatalf("build valid related-record operation: %v", err)
	}
	if payload.Doctype != "Deal" || len(payload.Records) != 2 || payload.Records[1].ExpectedRevision != 7 || payload.Records[1].TransitionAction != "Convert" {
		t.Fatalf("unexpected mutation bundle payload: %#v", payload)
	}
	dealItems := map[string]any{"records": []any{
		map[string]any{"key": "company", "doctype": "Company", "data": map[string]any{"company_name": "Northwind"}},
		map[string]any{"key": "deal", "doctype": "Deal", "data": map[string]any{
			"deal_name": "Northwind renewal",
			"items":     []any{map[string]any{"product_name": "Support renewal", "unit_price": float64(1200)}},
		}},
	}}
	if _, err := buildAIMutationBundle(reg, dealItems); err != nil {
		t.Fatalf("build related record with writable child rows: %v", err)
	}

	invalidCases := []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "missing revision", edit: func(update map[string]any) { delete(update, "expected_revision") }},
		{name: "hidden field", edit: func(update map[string]any) { update["data"] = map[string]any{"private_note": "secret"} }},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			bad := map[string]any{"records": []any{
				map[string]any{"key": "deal", "doctype": "Deal", "data": map[string]any{"deal_name": "Northwind renewal"}},
				map[string]any{"key": "lead", "doctype": "Lead", "operation": "update", "name": "LEAD-0001", "expected_revision": float64(7), "transition_action": "Convert", "data": map[string]any{"converted_deal": "$records.deal.name"}},
			}}
			tc.edit(bad["records"].([]any)[1].(map[string]any))
			if _, err := buildAIMutationBundle(reg, bad); err == nil {
				t.Fatal("invalid bundle was accepted")
			}
		})
	}
	badChild := map[string]any{"records": []any{
		map[string]any{"key": "company", "doctype": "Company", "data": map[string]any{"company_name": "Northwind"}},
		map[string]any{"key": "deal", "doctype": "Deal", "data": map[string]any{
			"deal_name": "Northwind renewal",
			"items":     []any{map[string]any{"product_name": "Support renewal", "unit_price": float64(1200), "line_total": float64(1200)}},
		}},
	}}
	if _, err := buildAIMutationBundle(reg, badChild); err == nil {
		t.Fatal("computed child field was accepted in atomic create")
	}
}

func TestMutationBundleRecordsApprovalBeforeAnyBusinessWrite(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Company", Fields: []doctype.Field{{Fieldname: "company_name", Fieldtype: "Data"}}})
	reg.Register(&doctype.DocType{Name: "Contact", Fields: []doctype.Field{{Fieldname: "full_name", Fieldtype: "Data"}}})
	args := map[string]any{"records": []any{
		map[string]any{"key": "company", "doctype": "Company", "data": map[string]any{"company_name": "Northwind"}},
		map[string]any{"key": "contact", "doctype": "Contact", "data": map[string]any{"full_name": "Amina Njeri"}},
	}}
	fingerprint := approvalFingerprint("mutate_records", args)
	mock.ExpectQuery(`SELECT COUNT\(1\)[\s\S]*FROM _kora_ai_approval[\s\S]*state = 'granted'[\s\S]*target_fingerprint = \?`).
		WithArgs("site-a", "run-1", "mutate_records", fingerprint).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_ai_approval`).
		WithArgs(sqlmock.AnyArg(), "site-a", "run-1", "alice", "agent", "mutate_records", "pending_approval", fingerprint, fingerprint, 0, sqlmock.AnyArg(), nil, nil, "", "").
		WillReturnResult(sqlmock.NewResult(1, 1))

	tx := &orm.TxManager{DB: database, Dialect: db.Resolve("mysql"), SiteName: "site-a", Context: context.Background()}
	result := executeAIMutationBundle(tx, reg, args, "alice", "site-a", "run-1", "", "bundle-key", testAIExecution(t, "site-a", "alice", []string{doctype.AdminRole}))
	if !strings.Contains(result, "Approval required") || !strings.Contains(result, "no records were changed") {
		t.Fatalf("unapproved bundle result = %q", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet durable-approval expectations (business writes must not run): %v", err)
	}
}

func TestAIReadAndToolProjectionsExcludeHiddenAndPasswordFields(t *testing.T) {
	dt := &doctype.DocType{Name: "Account", Fields: []doctype.Field{
		{Fieldname: "display_name", Fieldtype: "Data", InListView: true},
		{Fieldname: "private_note", Fieldtype: "Data", Hidden: true, InListView: true},
		{Fieldname: "credential", Fieldtype: "Password"},
	}}
	visible := aiReadableDocumentFields(dt, map[string]any{
		"display_name": "Public name", "private_note": "secret note", "credential": "secret password",
	})
	if len(visible) != 1 || visible["display_name"] != "Public name" {
		t.Fatalf("AI document projection = %#v, want only visible display_name", visible)
	}
	if summary := formatDocSummary(dt, &doctype.Document{Name: "ACC-1", Fields: map[string]any{
		"display_name": "Public name", "private_note": "secret note", "credential": "secret password",
	}}, 1); strings.Contains(summary, "secret") {
		t.Fatalf("AI summary exposed a hidden or password value: %q", summary)
	}
	tools := buildDoctypeToolDescriptors(dt)
	for _, tool := range tools {
		if strings.Contains(tool.Description, "private_note") || strings.Contains(tool.Description, "credential") {
			t.Fatalf("tool %s description exposed a hidden field: %q", tool.Name, tool.Description)
		}
		for _, hint := range tool.FieldHints {
			if hint.Name == "private_note" || hint.Name == "credential" {
				t.Fatalf("tool %s field hints exposed %q", tool.Name, hint.Name)
			}
		}
		if tool.InputSchema != nil {
			if properties, ok := tool.InputSchema["properties"].(map[string]any); ok {
				if _, found := properties["private_note"]; found {
					t.Fatalf("tool %s schema exposed hidden field", tool.Name)
				}
				if _, found := properties["credential"]; found {
					t.Fatalf("tool %s schema exposed password field", tool.Name)
				}
			}
		}
	}
	if unknown := unknownFields(map[string]any{"private_note": "try"}, dt); len(unknown) != 1 {
		t.Fatalf("hidden field mutation validation accepted a hidden field: %#v", unknown)
	}
	if _, _, _, _, err := buildValidatedFindArgs(dt, map[string]any{"filters": []any{map[string]any{"field": "private_note", "op": "=", "value": "secret note"}}}); err == nil {
		t.Fatal("AI find allowed filtering on a hidden field")
	}
}

func testAIExecution(t *testing.T, site, user string, roles []string) policy.Execution {
	t.Helper()
	execution, err := policy.NewVerifiedExecution(contract.TenantID(site), contract.ActorContext{
		PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user,
		Site: site, Roles: roles, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("create verified AI execution:", err)
	}
	return execution
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

func TestAIAssigneeReadsUseVerifiedUserScope(t *testing.T) {
	dt := &doctype.DocType{
		Name:      "Task",
		SortField: "modified",
		SortOrder: "DESC",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
			{Fieldname: "assigned_to", Fieldtype: "User"},
		},
	}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: "Assignee"}}, []*doctype.Permission{
		{Doctype: "Task", Role: "Assignee", Read: true, IfAssigned: []string{"assigned_to"}},
	})

	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	tx := &orm.TxManager{DB: database, Dialect: db.Resolve("mysql"), Context: context.Background()}
	alice := testAIExecution(t, "tenant-a", "alice", []string{"Assignee"})
	mock.ExpectQuery("SELECT title, assigned_to, name, owner, creation, modified, modified_by, doc_status, revision FROM .* WHERE name = \\? AND \\(`assigned_to` = \\?\\)").
		WithArgs("TASK-1", "alice").
		WillReturnRows(sqlmock.NewRows([]string{"title", "assigned_to", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).
			AddRow("Call customer", "alice", "TASK-1", "creator", nil, nil, nil, 0, 1))
	got := executeSingleToolWithRolesAndCallIDAndExecution(tx, registry, "task_get", map[string]any{"name": "TASK-1"}, "forged-user", "tenant-a", "run-1", "step-1", "call-1", nil, alice)
	if strings.Contains(got, "Permission denied") || !strings.Contains(got, "Call customer") {
		t.Fatalf("assigned AI get = %q, want the currently assigned Task", got)
	}

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM .* WHERE 1=1 AND \\(`assigned_to` = \\?\\)").
		WithArgs("alice").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT .* FROM .* WHERE 1=1 AND \\(`assigned_to` = \\?\\) ORDER BY .*modified.* DESC LIMIT \\? OFFSET \\?").
		WithArgs("alice", 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"title", "assigned_to", "name", "owner", "creation", "modified", "modified_by", "doc_status"}).
			AddRow("Call customer", "alice", "TASK-1", "creator", nil, nil, nil, 0))
	got = executeSingleToolWithRolesAndCallIDAndExecution(tx, registry, "task_list", map[string]any{}, "forged-user", "tenant-a", "run-1", "step-2", "call-2", nil, alice)
	if strings.Contains(got, "Permission denied") || !strings.Contains(got, "TASK-1") {
		t.Fatalf("assigned AI list = %q, want the assigned Task only", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet assignment-scoped AI read expectations:", err)
	}
}

func TestAIRecordMutationUsesVerifiedExecutionIdentity(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}},
		[]*doctype.Role{{Name: doctype.AdminRole}, {Name: "Creator"}},
		[]*doctype.Permission{{Doctype: "Task", Role: doctype.AdminRole, Create: true}})
	execution, err := policy.NewVerifiedExecution("stable-tenant-id", contract.ActorContext{
		PrincipalID: "verified-user", PrincipalType: contract.PrincipalHuman, Site: "site-a", Roles: []string{"Creator"}, AuthenticatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tx := &orm.TxManager{DB: database, Dialect: db.Resolve("mysql"), SiteName: "site-a"}
	_, err = executeAIRecordMutationWithExecution(tx, registry, kernel.CommandRecordCreate, "Task", "", map[string]any{"title": "x"}, "forged-owner", []string{doctype.AdminRole}, "", false, execution)
	if err == nil || err.Error() != "not permitted" {
		t.Fatalf("result error = %v, want the existing AI permission guard to deny before mutation", err)
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

func TestBuildValidatedReadArgumentsBoundEveryQueryDimension(t *testing.T) {
	dt := &doctype.DocType{Name: "Task", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data"},
		{Fieldname: "priority", Fieldtype: "Int"},
	}}
	tooManyFilters := make([]any, 21)
	for i := range tooManyFilters {
		tooManyFilters[i] = map[string]any{"field": "title", "op": "=", "value": "x"}
	}
	tooManyValues := make([]any, 101)
	for i := range tooManyValues {
		tooManyValues[i] = float64(i)
	}
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "fractional limit", args: map[string]any{"limit": 2.5}},
		{name: "oversized limit", args: map[string]any{"limit": float64(101)}},
		{name: "unbounded offset", args: map[string]any{"offset": float64(10001)}},
		{name: "too many filters", args: map[string]any{"filters": tooManyFilters}},
		{name: "oversized in list", args: map[string]any{"filters": []any{map[string]any{"field": "priority", "op": "in", "value": tooManyValues}}}},
		{name: "oversized filter text", args: map[string]any{"filters": []any{map[string]any{"field": "title", "op": "=", "value": strings.Repeat("x", 4097)}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, _, err := buildValidatedFindArgs(dt, tt.args); err == nil {
				t.Fatal("expected query argument to be rejected")
			}
		})
	}

	for _, tt := range []struct {
		name string
		args map[string]any
	}{
		{name: "fractional list limit", args: map[string]any{"limit": 1.5}},
		{name: "oversized list limit", args: map[string]any{"limit": float64(101)}},
		{name: "unbounded list offset", args: map[string]any{"offset": float64(10001)}},
		{name: "unexpected list argument", args: map[string]any{"order_by": "title"}},
	} {
		t.Run("list/"+tt.name, func(t *testing.T) {
			if _, _, err := buildValidatedListArgs(tt.args); err == nil {
				t.Fatal("expected list argument to be rejected")
			}
		})
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

func TestExecuteSingleToolScriptCreateRequiresAdministrator(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tx := &orm.TxManager{DB: db, SiteName: "site-a", Execution: testAIExecution(t, "site-a", "alice", nil)}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now())
	got := executeSingleTool(tx, doctype.NewRegistry(), "script_create", map[string]any{
		"name":        "hello_script",
		"script_type": "validate",
		"script":      "return true;",
	}, "alice", "site-a", "run-1", "")
	if !strings.Contains(got, "only a workspace Administrator") {
		t.Fatalf("expected administrator boundary, got %q", got)
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

	tx := &orm.TxManager{DB: db, SiteName: "site-a", Execution: testAIExecution(t, "site-a", "alice", nil)}
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

func TestAIScriptDraftIsInactiveAndAttributed(t *testing.T) {
	now := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
	draft := newAIScriptDraft(map[string]any{
		"name":        "notify_overdue",
		"script_type": "scheduled",
		"script":      "return true;",
		"schedule":    "0 9 * * *",
	}, "tenant-a", "admin@example.test", now)
	if draft.IsActive {
		t.Fatal("AI-authored script draft must remain inactive until reviewed")
	}
	if draft.CreatedBy != "admin@example.test" || draft.UpdatedBy != draft.CreatedBy || draft.Site != "tenant-a" {
		t.Fatalf("AI-authored draft attribution/scope = %#v", draft)
	}
	if !draft.CreatedAt.Equal(now) || !draft.UpdatedAt.Equal(now) {
		t.Fatalf("AI-authored draft timestamps = created %v, updated %v; want %v", draft.CreatedAt, draft.UpdatedAt, now)
	}
}

func TestAdministratorMayAuthorScriptDraftWhenStoreIsUnavailable(t *testing.T) {
	tx := &orm.TxManager{
		SiteName:  "site-a",
		Execution: testAIExecution(t, "site-a", "admin@example.test", []string{doctype.AdminRole}),
	}
	tx.Context = context.WithValue(context.Background(), "session_created_at", time.Now())
	got := executeSingleTool(tx, doctype.NewRegistry(), "script_create", map[string]any{
		"name": "notify_overdue", "script_type": "scheduled", "script": "return true;", "schedule": "0 9 * * *",
	}, "admin@example.test", "site-a", "run-1", "step-1")
	if !strings.Contains(got, "script store is not available") {
		t.Fatalf("Administrator request did not pass authorization to the draft writer: %q", got)
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

	tx := &orm.TxManager{DB: database, SiteName: "site-a", Execution: testAIExecution(t, "site-a", "alice", nil)}
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

	tx := &orm.TxManager{DB: database, SiteName: "site-a", Execution: testAIExecution(t, "site-a", "alice", nil)}
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
	mock.ExpectQuery(`SELECT config, version FROM _kora_config_version WHERE site = \? AND id = \?`).
		WithArgs("site-a", "cv-base-1").
		WillReturnRows(sqlmock.NewRows([]string{"config", "version"}).AddRow(`{"doctypes":[]}`, 1))
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

	tx := &orm.TxManager{DB: database, Dialect: db.Resolve("mysql"), SiteName: "site-a", Execution: testAIExecution(t, "site-a", "alice", []string{doctype.AdminRole})}
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

	tx := &orm.TxManager{DB: db, SiteName: "site-a", Execution: testAIExecution(t, "site-a", "alice", nil)}
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

	mock.ExpectExec(`INSERT INTO _kora_ai_audit`).
		WithArgs(
			sqlmock.AnyArg(),
			"site-a",
			"run-1",
			"step-1",
			"conv-1",
			"tool_call",
			"script_create",
			"failed",
			"user-1",
			"sid-1",
			"corr-1",
			"idem-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	tx := &orm.TxManager{DB: db, SiteName: "site-a", Execution: testAIExecution(t, "site-a", "alice", nil)}
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
	if !strings.Contains(content, "only a workspace Administrator") {
		t.Fatalf("expected administrator boundary, got %q", content)
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
