package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/policy"
)

const importOpRef = "stock.receiving~stock.receipt~receive"

// importOpFixture is a module-owned local operation backed by a config command
// (Input.Record = ImpReceipt), plus an external and an unbacked operation.
type importOpFixture struct {
	h        *Handler
	reg      *doctype.Registry
	scope    contract.BusinessScope
	mu       sync.Mutex
	calls    []contract.CommandMeta
	input    [][]byte
	seen     map[string]contract.OperationID
	commands *kernel.CommandRegistry
	// claim records a row key and reports whether it was already executed.
	claim func(key string) (contract.OperationID, bool)
}

func newImportOpFixture(t testing.TB, roles ...string) *importOpFixture {
	t.Helper()
	f := &importOpFixture{scope: contract.BusinessScope{TenantID: "imp-op", Kind: contract.ScopeKindTenant}, seen: map[string]contract.OperationID{}}
	in, out := contract.SchemaRef{ID: "stock.receive.input", Version: 1}, contract.SchemaRef{ID: "stock.receive.output", Version: 1}
	pub := func(op string) contract.PublishedOperation {
		return contract.PublishedOperation{Name: op, Capability: "stock.receipt", Operation: op, ScopePolicy: contract.ScopeTenantShared}
	}
	spec := func(op string, eff contract.OperationEffect) contract.OperationSpec {
		return contract.OperationSpec{Name: op, Input: in, Output: out, Effect: eff}
	}
	module := contract.ModuleDescriptor{ID: "stock.receiving", Version: "1.0.0",
		PublishedOperations: []contract.PublishedOperation{pub("receive"), pub("notify"), pub("bare")},
		Provides: []contract.CapabilityOffer{{Name: "stock.receipt", Contract: out, Operations: []contract.OperationSpec{
			spec("receive", contract.EffectLocal), spec("notify", contract.EffectExternal), spec("bare", contract.EffectLocal)}}}}
	snapshot := contract.ModuleSnapshot{TenantID: f.scope.TenantID, Revision: 3, Modules: []contract.ModuleDescriptor{module}}
	f.reg = doctype.NewRegistry()
	f.reg.Register(&doctype.DocType{Name: "ImpReceipt", ModuleOwnerID: "stock.receiving", ModuleOwnershipMode: string(contract.MutationCommands), Fields: []doctype.Field{{Fieldname: "sku", Fieldtype: "Data"}, {Fieldname: "qty", Fieldtype: "Int"}}})
	f.reg.Register(&doctype.DocType{Name: "ImpCrud", ModuleOwnerID: "stock.receiving", ModuleOwnershipMode: string(contract.MutationCRUD), Fields: []doctype.Field{{Fieldname: "sku", Fieldtype: "Data"}}})
	f.reg.Permissions.SetPermission(&doctype.Permission{Doctype: "ImpCrud", Role: "Stocker", Read: true, Create: true, Import: true})
	f.reg.Permissions.SetPermission(&doctype.Permission{Doctype: "ImpCrud", Role: "ReadOnly", Read: true, Import: true})
	for _, op := range []string{"receive", "notify", "bare"} {
		f.reg.Permissions.SetPermission(&doctype.Permission{Doctype: "module.stock.receiving.operation." + op, Role: "Clerk", Write: true})
	}
	commands := kernel.NewCommandRegistry()
	f.commands = commands
	for _, op := range []string{"receive", "notify"} {
		if err := commands.Register(&kernel.CommandResource{Name: "receive_" + op, Version: 1, ModuleOwnerID: "stock.receiving", Capability: "stock.receipt", Operation: op, InputSchema: in, OutputSchema: out,
			Input: kernel.CommandInput{Record: "ImpReceipt"}, Steps: []kernel.CommandStep{{Create: &kernel.StepCreate{Record: "ImpReceipt", Values: map[string]string{"sku": "$input.sku"}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	services := NewSiteRuntimeServices()
	services.Replace(string(f.scope.TenantID), SiteRuntimeService{KernelCommands: commands,
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		ExecutePublishedOperation: func(_ context.Context, _ policy.Execution, scope contract.BusinessScope, owner contract.ModuleID, p contract.PublishedOperation, _ contract.SchemaRef, meta contract.CommandMeta, input []byte) (contract.OperationReceipt, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			id, replay := f.claim(string(meta.IdempotencyKey))
			if !replay {
				f.calls = append(f.calls, meta)
				f.input = append(f.input, input)
			}
			ref := out
			return contract.OperationReceipt{ExecutionID: id, OperationRef: contract.PublishedOperationRef(owner, p.Capability, p.Operation), BusinessScope: scope, Status: contract.ReceiptCompleted, OutputSchema: &ref, Output: json.RawMessage(`{}`), Revision: "3"}, nil
		}})
	f.claim = func(key string) (contract.OperationID, bool) {
		_, replay := f.seen[key]
		f.seen[key] = contract.OperationID("exec-" + key[:12])
		return f.seen[key], replay
	}
	f.h = NewHandler(f.reg, nil)
	f.h.RuntimeServices = services
	return f
}

func (f *importOpFixture) post(t testing.TB, roles []string, spec, csv string) (int, map[string]any) {
	t.Helper()
	exec, err := policy.NewVerifiedExecution(f.scope.TenantID, contract.ActorContext{PrincipalID: "u", PrincipalType: contract.PrincipalHuman, Site: string(f.scope.TenantID), Roles: roles, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return f.postWith(t, exec, spec, csv)
}

func (f *importOpFixture) postWith(t testing.TB, exec policy.Execution, spec, csv string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = newImportMultipart(t, spec, csv)
	c.Set("site_name", string(f.scope.TenantID))
	c.Set("policy_execution", exec)
	f.h.HandleImport(c)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestImportOperationTarget(t *testing.T) {
	f := newImportOpFixture(t)
	spec := func(extra string) string {
		return `{"import_id":"i1",` + extra + `"mapping":{"SKU":"sku","Qty":"qty"}}`
	}
	clerk := []string{"Clerk"}
	for _, tt := range []struct {
		name, spec string
		roles      []string
		status     int
		code       string
	}{
		{"no permission", spec(`"target":{"operation_ref":"` + importOpRef + `"},`), []string{"Other"}, 403, "permission.denied"},
		{"external effect", spec(`"target":{"operation_ref":"stock.receiving~stock.receipt~notify"},`), clerk, 400, ""},
		{"no backing command", spec(`"target":{"operation_ref":"stock.receiving~stock.receipt~bare"},`), clerk, 400, ""},
		{"unregistered", spec(`"target":{"operation_ref":"x~y~z"},`), clerk, 400, "import.target_unsupported"},
		{"key_fields", spec(`"target":{"operation_ref":"` + importOpRef + `","key_fields":["sku"]},`), clerk, 400, "import.unsupported"},
		{"both targets", spec(`"target":{"operation_ref":"` + importOpRef + `","doctype":"ImpReceipt"},`), clerk, 400, "import.target_invalid"},
		{"commands_only doctype lists operations", spec(`"target":{"doctype":"ImpReceipt"},`), clerk, 400, "import.target_owned"},
		{"commands_only refused even with grants", spec(`"target":{"doctype":"ImpReceipt"},`), []string{"Stocker"}, 400, "import.target_owned"},
		{"crud-owned without create is refused by permission", spec(`"target":{"doctype":"ImpCrud"},`), []string{"ReadOnly"}, 403, "permission.denied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, body := f.post(t, tt.roles, tt.spec, "SKU,Qty\na,1\n")
			raw, _ := json.Marshal(body)
			want := tt.code
			if want == "" {
				want = "import.target_unsupported"
			}
			if status != tt.status || !strings.Contains(string(raw), want) {
				t.Fatalf("got %d %s", status, raw)
			}
			if tt.name == "commands_only doctype lists operations" && !strings.Contains(string(raw), importOpRef) {
				t.Fatalf("owned details lack %s: %s", importOpRef, raw)
			}
		})
	}
	if len(f.calls) != 0 {
		t.Fatalf("rejections executed %d calls", len(f.calls))
	}
}

func TestImportOperationRunsPerRowWithRowKeys(t *testing.T) {
	f := newImportOpFixture(t)
	spec := `{"import_id":"i1","target":{"operation_ref":"` + importOpRef + `"},"mapping":{"SKU":"sku","Qty":"qty"}`
	csv := "SKU,Qty\na,1\nb,2\nc,x\n"
	status, dry := f.post(t, []string{"Clerk"}, spec+`,"dry_run":true}`, csv)
	if status != 200 || dry["executed"] != false || dry["succeeded"] != float64(2) || dry["failed"] != float64(1) || len(f.calls) != 0 {
		t.Fatalf("dry run %d %v calls=%d", status, dry, len(f.calls))
	}
	status, out := f.post(t, []string{"Clerk"}, spec+`}`, csv)
	if status != 200 || out["succeeded"] != float64(2) || len(f.calls) != 2 || f.calls[1].IdempotencyKey != contract.IdempotencyKey(kernel.BulkRowKey("i1", 2)) {
		t.Fatalf("run %d %v calls=%d", status, out, len(f.calls))
	}
	if string(f.input[0]) != `{"qty":"1","sku":"a"}` {
		t.Fatalf("input %s", f.input[0])
	}
	if _, again := f.post(t, []string{"Clerk"}, spec+`}`, csv); again["succeeded"] != float64(2) || len(f.calls) != 2 {
		t.Fatalf("replay re-executed: %v calls=%d", again, len(f.calls))
	}
}

func TestImportOperationGroupsToOneCall(t *testing.T) {
	f := newImportOpFixture(t)
	f.reg.Register(&doctype.DocType{Name: "ImpLine", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "sku", Fieldtype: "Data"}}})
	f.reg.Register(&doctype.DocType{Name: "ImpGrouped", ModuleOwnerID: "stock.receiving", Fields: []doctype.Field{{Fieldname: "ref", Fieldtype: "Data"}, {Fieldname: "lines", Fieldtype: "Table", Options: "ImpLine"}}})
	cmd, _ := f.commands.LookupCapability("stock.receipt", "receive")
	cmd.Input.Record = "ImpGrouped"
	spec := `{"import_id":"g1","group_by":"Ref","target":{"operation_ref":"` + importOpRef + `"},"mapping":{"Ref":"ref","SKU":"lines.sku"}}`
	status, out := f.post(t, []string{"Clerk"}, spec, "Ref,SKU\nR1,a\nR1,b\nR1,c\n")
	var in struct {
		Lines []map[string]any `json:"lines"`
	}
	if status != 200 || out["total"] != float64(1) || len(f.input) != 1 || json.Unmarshal(f.input[0], &in) != nil || len(in.Lines) != 3 {
		t.Fatalf("group %d %v inputs=%d %s", status, out, len(f.input), f.input)
	}
}

func newImportMultipart(t testing.TB, spec, csv string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("spec", spec)
	fw, _ := w.CreateFormFile("file", "x.csv")
	_, _ = fw.Write([]byte(csv))
	_ = w.Close()
	r := httptest.NewRequest(http.MethodPost, "/import", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}
