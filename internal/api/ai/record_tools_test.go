package ai

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// recordToolsRegistry holds a plain type, a type only Administrator reads, a child table, a crud
// module type and a commands-only module type. Creator reads Task and Item only.
func recordToolsRegistry() *doctype.Registry {
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{
		{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}},
		{Name: "Secret Note", Fields: []doctype.Field{{Fieldname: "body", Fieldtype: "Data"}}},
		{Name: "Task Line", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "qty", Fieldtype: "Int"}}},
		{Name: "Item", ModuleOwnerID: "inventory.core", ModuleOwnershipMode: "crud", ModuleAccess: contract.ModulePublished, Fields: []doctype.Field{{Fieldname: "item_name", Fieldtype: "Data", Reqd: true}}},
		{Name: "Sale", ModuleOwnerID: "pos.core", ModuleOwnershipMode: "commands_only", ModuleAccess: contract.ModulePublished, Fields: []doctype.Field{{Fieldname: "total", Fieldtype: "Currency"}}},
	}, []*doctype.Role{{Name: doctype.AdminRole}, {Name: "Creator"}}, []*doctype.Permission{
		{Doctype: "Task", Role: doctype.AdminRole, Read: true, Create: true, Write: true},
		{Doctype: "Secret Note", Role: doctype.AdminRole, Read: true},
		{Doctype: "Item", Role: doctype.AdminRole, Read: true, Create: true, Write: true},
		{Doctype: "Sale", Role: doctype.AdminRole, Read: true, Create: true},
		{Doctype: "Task", Role: "Creator", Read: true, Create: true, Write: true},
		{Doctype: "Item", Role: "Creator", Read: true},
	})
	return reg
}

// recordTypeEnum returns the record_type enum of the find_records tool in a catalog.
func recordTypeEnum(t *testing.T, catalog ToolCatalog) []string {
	t.Helper()
	tool := findTool(t, catalog.Tools, toolFindRecords)
	props := tool.InputSchema["properties"].(map[string]any)
	return props["record_type"].(map[string]any)["enum"].([]string)
}

func TestRecordToolEnumFollowsReadPermission(t *testing.T) {
	reg := recordToolsRegistry()
	for _, tc := range []struct {
		name  string
		opts  CatalogOptions
		want  []string
		notIn []string
	}{
		{name: "service catalog lists every non-child type", opts: CatalogOptions{}, want: []string{"Item", "Sale", "Secret Note", "Task"}, notIn: []string{"Task Line"}},
		{name: "administrator reads every non-child type", opts: CatalogOptions{Execution: testAIExecution(t, "tenant-a", "admin", []string{doctype.AdminRole})}, want: []string{"Item", "Sale", "Secret Note", "Task"}, notIn: []string{"Task Line"}},
		{name: "creator sees only the types it may read", opts: CatalogOptions{Execution: testAIExecution(t, "tenant-a", "creator", []string{"Creator"})}, want: []string{"Item", "Task"}, notIn: []string{"Sale", "Secret Note", "Task Line"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enum := recordTypeEnum(t, BuildToolCatalog(reg, tc.opts))
			if !slices.IsSorted(enum) {
				t.Fatalf("enum %v is not sorted", enum)
			}
			if !slices.Equal(enum, tc.want) {
				t.Fatalf("enum = %v, want %v", enum, tc.want)
			}
			for _, name := range tc.notIn {
				if slices.Contains(enum, name) {
					t.Fatalf("enum %v includes %q", enum, name)
				}
			}
		})
	}
}

func TestRecordToolCatalogIsFixedAndStable(t *testing.T) {
	reg := recordToolsRegistry()
	opts := CatalogOptions{Execution: testAIExecution(t, "tenant-a", "admin", []string{doctype.AdminRole})}
	first := BuildToolCatalog(reg, opts)
	second := BuildToolCatalog(reg, opts)
	if first.Version != second.Version {
		t.Fatalf("catalog version changed between identical builds: %q vs %q", first.Version, second.Version)
	}
	for _, name := range []string{toolFindRecords, toolGetRecord, toolDescribeRecord, toolCreateRecord, toolUpdateRecord} {
		findTool(t, first.Tools, name)
	}
	for _, tool := range first.Tools {
		if tool.Operation == "system" {
			continue
		}
		if tool.Doctype != "" {
			t.Fatalf("tool %s is bound to doctype %q; record tools must be fixed", tool.Name, tool.Doctype)
		}
		if strings.HasSuffix(tool.Name, "_find") || strings.HasSuffix(tool.Name, "_create") || strings.HasSuffix(tool.Name, "_update") {
			t.Fatalf("per-type tool %s is still in the catalog", tool.Name)
		}
	}
	if got := BuildToolCatalog(doctype.NewRegistry(), CatalogOptions{}); len(got.Tools) != len(first.Tools)-6 {
		t.Fatalf("empty registry should drop the five record tools and confirm_write, got %d tools", len(got.Tools))
	}
}

func TestRecordToolTargetResolvesFixedToolsOnNonChildTypes(t *testing.T) {
	reg := recordToolsRegistry()
	for _, tc := range []struct {
		name, tool, recordType, wantType, wantOp string
		wantOK                                   bool
	}{
		{name: "find reads", tool: toolFindRecords, recordType: "Task", wantType: "Task", wantOp: "read", wantOK: true},
		{name: "describe reads", tool: toolDescribeRecord, recordType: "Item", wantType: "Item", wantOp: "read", wantOK: true},
		{name: "create creates", tool: toolCreateRecord, recordType: "Task", wantType: "Task", wantOp: "create", wantOK: true},
		{name: "update writes", tool: toolUpdateRecord, recordType: "Item", wantType: "Item", wantOp: "write", wantOK: true},
		{name: "child table is not a record type", tool: toolFindRecords, recordType: "Task Line"},
		{name: "unknown type", tool: toolCreateRecord, recordType: "Nope"},
		{name: "non-record tool", tool: "list_doctypes", recordType: "Task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docType, operation, ok := RecordToolTarget(reg, tc.tool, map[string]any{"record_type": tc.recordType})
			if ok != tc.wantOK || docType != tc.wantType || operation != tc.wantOp {
				t.Fatalf("target = %q/%q/%v, want %q/%q/%v", docType, operation, ok, tc.wantType, tc.wantOp, tc.wantOK)
			}
			if wantRecordTool := tc.tool != "list_doctypes"; IsRecordTool(tc.tool) != wantRecordTool {
				t.Fatalf("IsRecordTool(%q) = %v, want %v", tc.tool, !wantRecordTool, wantRecordTool)
			}
		})
	}
}

func TestModuleCommandRefusalNamesPackCommandsForCommandsOnlyTypes(t *testing.T) {
	reg := recordToolsRegistry()
	commands := kernel.NewCommandRegistry()
	if err := commands.Register(&kernel.CommandResource{Name: "record_sale", Namespace: "pos", ModuleOwnerID: "pos.core", Input: kernel.CommandInput{Record: "Sale"}}); err != nil {
		t.Fatal("register command:", err)
	}
	for _, tc := range []struct {
		name       string
		recordType string
		commands   *kernel.CommandRegistry
		wantRefuse bool
		wantNamed  string
	}{
		{name: "crud module type takes direct writes", recordType: "Item", commands: commands},
		{name: "commands-only type names its command", recordType: "Sale", commands: commands, wantRefuse: true, wantNamed: "pos.record_sale"},
		{name: "commands-only type without a registry still refuses", recordType: "Sale", wantRefuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal := moduleCommandRefusal(reg.Get(tc.recordType), tc.commands)
			if !tc.wantRefuse {
				if refusal != "" {
					t.Fatalf("refusal = %q, want direct write allowed", refusal)
				}
				return
			}
			if !strings.HasPrefix(refusal, `{"error":`) || !strings.Contains(refusal, `"code":"module_command_required"`) {
				t.Fatalf("refusal = %q, want structured module_command_required", refusal)
			}
			if tc.wantNamed != "" && !strings.Contains(refusal, tc.wantNamed) {
				t.Fatalf("refusal = %q, want command %q named", refusal, tc.wantNamed)
			}
		})
	}
}

func TestDescribeRecordTypeReportsWritableFieldsAndChildren(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{
		{Name: "Order Line", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "qty", Fieldtype: "Int", Reqd: true}}},
		{Name: "Order", Fields: []doctype.Field{
			{Fieldname: "status", Fieldtype: "Select", Options: "Open\nClosed", Default: "Open"},
			{Fieldname: "customer", Fieldtype: "Link", Options: "Customer"},
			{Fieldname: "total", Fieldtype: "Currency", Computed: "SUM(lines.qty)"},
			{Fieldname: "lines", Fieldtype: "Table", Options: "Order Line"},
		}},
	}, []*doctype.Role{{Name: doctype.AdminRole}}, nil)
	var described describedRecordType
	if err := json.Unmarshal([]byte(describeRecordType(reg, reg.Get("Order"), nil)), &described); err != nil {
		t.Fatal("decode describe_record_type:", err)
	}
	byName := map[string]describedField{}
	for _, field := range described.Fields {
		byName[field.Name] = field
	}
	if status := byName["status"]; !slices.Equal(status.Options, []string{"Open", "Closed"}) || status.Default != "Open" || !status.Writable {
		t.Fatalf("status field = %+v, want options, default and writable", status)
	}
	if customer := byName["customer"]; customer.LinkTarget != "Customer" {
		t.Fatalf("customer field = %+v, want link target Customer", customer)
	}
	if total := byName["total"]; total.Writable {
		t.Fatalf("computed total field is writable: %+v", total)
	}
	lines := byName["lines"]
	if len(lines.Children) != 1 || lines.Children[0].Name != "qty" || !lines.Children[0].Required {
		t.Fatalf("lines child fields = %+v, want required qty", lines.Children)
	}
	if len(described.Commands) != 0 {
		t.Fatalf("crud-only type lists pack commands: %v", described.Commands)
	}
}

func TestRecordToolExecutionRefusesChildAndUnknownTypes(t *testing.T) {
	reg := recordToolsRegistry()
	for _, tc := range []struct {
		name string
		args map[string]any
		tool string
		want string
	}{
		{name: "child table", tool: toolFindRecords, args: map[string]any{"record_type": "Task Line"}, want: "Unknown record type"},
		{name: "unknown type", tool: toolGetRecord, args: map[string]any{"record_type": "Nope", "name": "X"}, want: "Unknown record type"},
		{name: "role without create", tool: toolCreateRecord, args: map[string]any{"record_type": "Sale", "data": map[string]any{"total": 1}}, want: `"code":"permission_denied"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, _, err := sqlmock.New()
			if err != nil {
				t.Fatal("create SQL mock:", err)
			}
			defer database.Close()
			tx := &orm.TxManager{DB: database, Dialect: db.Resolve("mysql"), Context: context.Background()}
			execution := testAIExecution(t, "tenant-a", "creator", []string{"Creator"})
			got := executeSingleTool(tx, reg, toolInvocation{Name: tc.tool, Args: tc.args, Owner: "creator", Site: "tenant-a", Execution: execution})
			if !strings.Contains(got, tc.want) {
				t.Fatalf("result = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestTypedNameOpensOneTitle(t *testing.T) {
	tests := []struct {
		name    string
		typed   string
		matches orm.TitleMatches
		want    string
		ok      bool
	}{
		{name: "a code match of one record", typed: "sb-0001", matches: orm.TitleMatches{Kind: orm.TitleKindCode, Matches: []orm.TitleMatch{{Name: "SB-0001", Title: "SB-0001"}}}, want: "SB-0001", ok: true},
		{name: "one title equal ignoring case", typed: "bill-001", matches: orm.TitleMatches{Kind: orm.TitleKindWords, Matches: []orm.TitleMatch{{Name: "SB-0003", Title: "BILL-001"}}}, want: "SB-0003", ok: true},
		{name: "a title that only holds the words", typed: "bill-001", matches: orm.TitleMatches{Kind: orm.TitleKindWords, Matches: []orm.TitleMatch{{Name: "SB-0003", Title: "BILL-0010 spare"}}}},
		{name: "two titles equal to the text", typed: "bill-001", matches: orm.TitleMatches{Kind: orm.TitleKindWords, Matches: []orm.TitleMatch{{Name: "SB-0003", Title: "BILL-001"}, {Name: "SB-0004", Title: "bill-001"}}}},
		{name: "a typo match is not opened", typed: "bil-001", matches: orm.TitleMatches{Kind: orm.TitleKindTypo, Matches: []orm.TitleMatch{{Name: "SB-0003", Title: "BILL-001"}}}},
		{name: "no match", typed: "zzz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := typedName(tt.typed).openTitle(tt.matches)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("openTitle(%q) = %q, %v; want %q, %v", tt.typed, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestTypedNameNearCodes(t *testing.T) {
	codes := []string{"SUPP-0001", "SB-0001", "SUPP-0002", "SUPP-0003", "CUST-0009"}
	tests := []struct {
		name  string
		typed string
		want  []string
	}{
		{name: "a code one digit short gives only the nearest", typed: "supp-001", want: []string{"SUPP-0001"}},
		{name: "equally near codes all show, by code", typed: "SUPP-0004", want: []string{"SUPP-0001", "SUPP-0002", "SUPP-0003"}},
		{name: "a word with no near code", typed: "zzz"},
		{name: "a short word is too far from every code", typed: "sup-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := typedName(tt.typed).nearCodes(codes)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("nearCodes(%q) = %v, want %v", tt.typed, got, tt.want)
			}
		})
	}
}

// readOnlyBill is a Supplier Bill with two read-only fields: one with a description, one without.
func readOnlyBill() *doctype.DocType {
	return &doctype.DocType{Name: "Supplier Bill", Fields: []doctype.Field{
		{Fieldname: "reference", Fieldtype: "Data", Label: "Reference"},
		{Fieldname: "base_amount", Fieldtype: "Currency", Label: "Amount in base currency", ReadOnly: true},
		{Fieldname: "status", Fieldtype: "Select", Label: "Status", ReadOnly: true, Description: "Follows payments."},
		{Fieldname: "notes", Fieldtype: "Data", Label: "Notes"},
	}}
}

func TestUpdateRefusesReadOnlyFieldsWithReason(t *testing.T) {
	dt := readOnlyBill()
	for _, tc := range []struct {
		name   string
		args   map[string]any
		want   []string
		absent []string
	}{
		{name: "read-only field without description", args: map[string]any{"name": "SB-1", "base_amount": 5},
			want: []string{"Amount in base currency is read-only on Supplier Bill: it is set by the system.", `"field":"base_amount"`, "Leave base_amount out of data."}},
		{name: "read-only field with description", args: map[string]any{"name": "SB-1", "status": "Paid"},
			want: []string{"Status is read-only on Supplier Bill: Follows payments.", "Leave status out of data."}},
		{name: "two read-only fields are both named", args: map[string]any{"name": "SB-1", "status": "Paid", "base_amount": 5},
			want: []string{"Amount in base currency is read-only on Supplier Bill", "Status is read-only on Supplier Bill", "Leave base_amount, status out of data."}},
		{name: "writable fields only are not refused", args: map[string]any{"name": "SB-1", "reference": "R", "notes": "n"},
			absent: []string{"read-only"}},
		{name: "name is the target, not data", args: map[string]any{"name": "base_amount", "notes": "n"},
			absent: []string{"read-only"}},
		{name: "unknown field keeps the unknown error", args: map[string]any{"name": "SB-1", "missing": 1, "base_amount": 5},
			want: []string{"Unknown fields: missing."}, absent: []string{"read-only"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, refusal := updateChanges(dt, tc.args)
			got := ""
			if refusal != nil {
				got = refusal.text()
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("update refusal = %q, want it to contain %q", got, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Fatalf("update refusal = %q, want it not to contain %q", got, absent)
				}
			}
		})
	}
}

func TestCreateRefusesReadOnlyFieldsBeforeWrite(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{readOnlyBill()}, nil, nil)
	dt := reg.Get("Supplier Bill")
	for _, tc := range []struct {
		name   string
		data   map[string]any
		want   string
		absent string
	}{
		{name: "read-only field is refused", data: map[string]any{"reference": "R", "base_amount": 5},
			want: "Amount in base currency is read-only on Supplier Bill: it is set by the system."},
		{name: "writable fields are not refused", data: map[string]any{"reference": "R"}, absent: "read-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A nil transaction proves the refusal happens before any write or preview is attempted.
			got := executeRecordCreate(nil, reg, toolInvocation{Args: map[string]any{"data": tc.data}}, dt, policy.RowScope{})
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("create result = %q, want it to contain %q", got, tc.want)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Fatalf("create result = %q, want it not to contain %q", got, tc.absent)
			}
		})
	}
}
