package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// fakeOperations is a ModuleOperations over a fixed catalog. It records the last run request.
type fakeOperations struct {
	catalog    contract.ApplicationCatalog
	previews   int
	runErr     error
	receipt    contract.OperationReceipt
	lastRef    string
	lastInput  json.RawMessage
	lastKey    contract.IdempotencyKey
	lastSchema contract.SchemaRef
	runs       int
}

func (f *fakeOperations) Catalog(policy.Execution, contract.BusinessScope) (contract.ApplicationCatalog, error) {
	return f.catalog, nil
}

// Preview records the call and returns the same receipt, without counting it as a run.
func (f *fakeOperations) Preview(_ context.Context, _ policy.Execution, _ contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	f.previews++
	f.lastRef, f.lastInput = ref, request.Input
	return f.receipt, f.runErr
}

func (f *fakeOperations) Run(_ context.Context, _ policy.Execution, _ contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	f.runs++
	f.lastRef, f.lastInput, f.lastKey, f.lastSchema = ref, request.Input, request.Meta.IdempotencyKey, request.InputSchema
	return f.receipt, f.runErr
}

// posCommands registers one pack command that records a sale, owned by pos.core.
func posCommands(t *testing.T) *kernel.CommandRegistry {
	t.Helper()
	commands := kernel.NewCommandRegistry()
	if err := commands.Register(&kernel.CommandResource{Name: "record_sale", Namespace: "pos", ModuleOwnerID: "pos.core", Capability: "pos.checkout", Operation: "record_sale", Input: kernel.CommandInput{Record: "Sale"}}); err != nil {
		t.Fatal(err)
	}
	return commands
}

func packCatalog() contract.ApplicationCatalog {
	saleSchema := contract.SchemaRef{ID: "pos.core.pos.checkout.record_sale.input", Version: 1}
	return contract.ApplicationCatalog{
		Schemas: []contract.ModuleSchemaDocument{{Ref: saleSchema, Schema: json.RawMessage(`{"type":"object","additionalProperties":false,
			"required":["register"],"properties":{
				"register":{"type":"string","description":"Till register"},
				"status":{"type":"string","enum":["Paid","Held"]},
				"items":{"type":"array","items":{"type":"object","required":["product"],"properties":{"product":{"type":"string"},"quantity":{"type":"number"}}}}}}`)}},
		PublishedOperations: []contract.ApplicationCatalogOperation{
			{OperationRef: "pos.core~pos.checkout~record_sale", OwnerModuleID: "pos.core", Capability: "pos.checkout", Operation: "record_sale", InputSchema: saleSchema, Availability: contract.OperationAvailable, Presentation: contract.SurfacePresentation{Title: "Record sale"}},
			{OperationRef: "pos.core~pos.till~open_session", OwnerModuleID: "pos.core", Capability: "pos.till", Operation: "open_session", Availability: contract.OperationAvailable},
			{OperationRef: "pos.core~pos.till~close_session", OwnerModuleID: "pos.core", Capability: "pos.till", Operation: "close_session", Availability: contract.OperationDisabled},
		},
	}
}

func packInvocation(t *testing.T, ops ModuleOperations, name string, args map[string]any) string {
	t.Helper()
	tx := &orm.TxManager{Context: context.Background()}
	return executeSingleTool(tx, doctype.NewRegistry(), toolInvocation{
		Name: name, Args: args, Owner: "alice", Site: "site", RunID: "run", StepID: "step", IdempotencyKey: "ai-tool-key",
		Execution: testAIExecution(t, "site", "alice", []string{doctype.AdminRole}), Commands: posCommands(t), Operations: ops,
	})
}

func TestBuildToolCatalogOffersPackActionsOnlyWithOperations(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Item", Fields: []doctype.Field{{Fieldname: "item_name", Fieldtype: "Data"}}})
	for _, tc := range []struct {
		name string
		ops  ModuleOperations
		want []string
	}{
		{name: "no operations (MCP, channel default)", ops: nil, want: []string{"find_records", "get_record", "describe_record_type", "create_record", "update_record"}},
		{name: "with operations", ops: &fakeOperations{}, want: []string{"find_records", "get_record", "describe_record_type", "create_record", "update_record", "list_actions", "run_action"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := BuildToolCatalog(reg, CatalogOptions{Operations: tc.ops})
			var names []string
			for _, tool := range catalog.Tools {
				if tool.Source == "tenant" && IsRecordTool(tool.Name) || tool.Name == toolListActions || tool.Name == toolRunAction {
					names = append(names, tool.Name)
				}
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("tools = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestPackActionDescriptorsSafetyAndOperation(t *testing.T) {
	for _, tc := range []struct {
		name, safety, operation string
	}{
		{name: toolListActions, safety: "safe", operation: "list"},
		{name: toolRunAction, safety: "guarded", operation: "execute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, tool := range packActionDescriptors() {
				if tool.Name != tc.name {
					continue
				}
				if tool.SafetyLevel != tc.safety || tool.Operation != tc.operation {
					t.Fatalf("%s safety=%s operation=%s, want %s %s", tc.name, tool.SafetyLevel, tool.Operation, tc.safety, tc.operation)
				}
				return
			}
			t.Fatalf("%s is not described", tc.name)
		})
	}
}

func TestListPackActionsFilters(t *testing.T) {
	ops := &fakeOperations{catalog: packCatalog()}
	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{name: "all available", args: map[string]any{}, want: []string{"pos.core~pos.checkout~record_sale", "pos.core~pos.till~open_session"}},
		{name: "record_type matches the command", args: map[string]any{"record_type": "sale"}, want: []string{"pos.core~pos.checkout~record_sale"}},
		{name: "record_type without a command matches nothing", args: map[string]any{"record_type": "Item"}, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Actions []packAction `json:"actions"`
			}
			if err := json.Unmarshal([]byte(packInvocation(t, ops, toolListActions, tc.args)), &got); err != nil {
				t.Fatalf("decode list_actions: %v", err)
			}
			var refs []string
			for _, action := range got.Actions {
				refs = append(refs, action.Action)
			}
			if strings.Join(refs, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("actions = %v, want %v", refs, tc.want)
			}
		})
	}
}

func TestDescribePackActionTrimsInputSchema(t *testing.T) {
	ops := &fakeOperations{catalog: packCatalog()}
	out := packInvocation(t, ops, toolListActions, map[string]any{"action": "pos.core~pos.checkout~record_sale"})
	var got struct {
		Action     string `json:"action"`
		Title      string `json:"title"`
		RecordType string `json:"record_type"`
		Input      struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type        string   `json:"type"`
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
				Required    []string `json:"required"`
				Items       *struct {
					Required   []string `json:"required"`
					Properties map[string]struct {
						Type string `json:"type"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"properties"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode describe: %v: %s", err, out)
	}
	if got.Title != "Record sale" || got.RecordType != "Sale" || got.Input.Required[0] != "register" {
		t.Fatalf("describe = %s", out)
	}
	if got.Input.Properties["status"].Enum[1] != "Held" || got.Input.Properties["register"].Description != "Till register" {
		t.Fatalf("enum or description dropped: %s", out)
	}
	items := got.Input.Properties["items"].Items
	if items == nil || items.Required[0] != "product" || items.Properties["quantity"].Type != "number" {
		t.Fatalf("child row fields dropped: %s", out)
	}
	if strings.Contains(out, "additionalProperties") {
		t.Fatalf("keywords the caller does not fill were kept: %s", out)
	}
}

func TestRunPackActionRefusalsAndReplay(t *testing.T) {
	ops := &fakeOperations{catalog: packCatalog(), receipt: contract.OperationReceipt{ExecutionID: "exec-1", Status: contract.ReceiptCompleted}}
	for _, tc := range []struct {
		name    string
		args    map[string]any
		runErr  error
		runs    int
		wantSub string
	}{
		{name: "unknown action", args: map[string]any{"action": "pos.core~pos.till~missing", "input": map[string]any{}}, wantSub: `"code":"not_found"`},
		{name: "unavailable action", args: map[string]any{"action": "pos.core~pos.till~close_session", "input": map[string]any{}}, wantSub: `"code":"not_found"`},
		{name: "input is not an object", args: map[string]any{"action": "pos.core~pos.till~open_session", "input": "float 500"}, wantSub: `"code":"invalid_value"`},
		{name: "executor refusal reaches the model", args: map[string]any{"action": "pos.core~pos.till~open_session", "input": map[string]any{}}, runErr: OperationError{Code: "validation.failed", Message: "Till is not open", Field: "till_session"}, runs: 1, wantSub: `"field":"till_session"`},
		{name: "completed action", args: map[string]any{"action": "pos.core~pos.checkout~record_sale", "input": map[string]any{"register": "R1"}}, runs: 1, wantSub: `"execution_id":"exec-1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops.runs, ops.runErr = 0, tc.runErr
			out := packInvocation(t, ops, toolRunAction, tc.args)
			if !strings.Contains(out, tc.wantSub) {
				t.Fatalf("result = %s, want %s", out, tc.wantSub)
			}
			if ops.runs != tc.runs {
				t.Fatalf("runs = %d, want %d", ops.runs, tc.runs)
			}
		})
	}
}

func TestRunPackActionPassesSchemaAndToolCallKey(t *testing.T) {
	ops := &fakeOperations{catalog: packCatalog(), receipt: contract.OperationReceipt{ExecutionID: "exec-1", Status: contract.ReceiptCompleted}}
	packInvocation(t, ops, toolRunAction, map[string]any{"action": "pos.core~pos.checkout~record_sale", "input": map[string]any{"register": "R1"}})
	if ops.lastRef != "pos.core~pos.checkout~record_sale" || ops.lastKey != "ai-tool-key" || ops.lastSchema.ID != "pos.core.pos.checkout.record_sale.input" {
		t.Fatalf("run got ref=%q key=%q schema=%+v", ops.lastRef, ops.lastKey, ops.lastSchema)
	}
	if string(ops.lastInput) != `{"register":"R1"}` {
		t.Fatalf("run input = %s", ops.lastInput)
	}
}

func TestPackActionsRefuseWithoutOperations(t *testing.T) {
	out := packInvocation(t, nil, toolRunAction, map[string]any{"action": "x", "input": map[string]any{}})
	if !strings.Contains(out, `"code":"permission_denied"`) {
		t.Fatalf("result = %s, want a refusal", out)
	}
}

func TestOperationErrorIsAnError(t *testing.T) {
	var err error = OperationError{Code: "c", Message: "m"}
	var got OperationError
	if !errors.As(err, &got) || got.Code != "c" {
		t.Fatal("OperationError does not unwrap as itself")
	}
}
