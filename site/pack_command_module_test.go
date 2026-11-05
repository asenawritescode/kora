package site

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/query"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const saleYAML = `
name: record_sale
namespace: shop
version: 1
input: {record: Sale}
module_owner_id: shop.core
capability: shop.sale_commands
operation: record_sale
offline_eligible: true
presentation: {layout: line_entry, line_entry: {lines_path: items, lookup_query: "shop.core~shop.items~items", lookup_input: name, item_map: {item: name}, quantity_field: quantity}}
transaction:
  - as: sale
    create: {record: Sale, values: {register: $input.register, items: $input.items, payments: $input.payments}}
  - for_each: $input.items
    create: {record: Sale Line, values: {item: $item.item, sale: $steps.sale.name}}
output: {reference: $steps.sale.name}
`

const shiftYAML = `
name: open_shift
namespace: shop
version: 1
input: {record: Shift}
module_owner_id: shop.core
capability: shop.shift_commands
operation: open_shift
transaction:
  - create: {record: Shift, values: {register: $input.register}}
`

func packCommandFixture(t *testing.T, cmdYAML ...string) *doctype.ConfigSnapshot {
	t.Helper()
	snap := packQueries(t, query.Definition{Name: "items", ModuleOwnerID: "shop.core", Capability: "shop.items", Operation: "items", Source: "Item", Scope: "tenant", Fields: []string{"name"}})
	f := func(n, ft, o string) doctype.Field { return doctype.Field{Fieldname: n, Fieldtype: ft, Options: o} }
	snap.DocTypes = []*doctype.DocType{
		{Name: "Sale", Fields: []doctype.Field{f("register", "Link", "Register"), f("items", "Table", "Sale Item"), f("payments", "Table", "Sale Payment")}},
		{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{f("item", "Link", "Item"), f("quantity", "Float", "")}},
		{Name: "Sale Payment", IsChildTable: true, Fields: []doctype.Field{f("amount", "Currency", "")}},
		{Name: "Shift", Fields: []doctype.Field{f("register", "Link", "Register")}},
	}
	for _, y := range cmdYAML {
		def, err := kernel.ParseCommandResource([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := kernel.EncodeCommandResources([]*kernel.CommandResource{def})
		snap.Commands = append(snap.Commands, raw...)
	}
	return snap
}

func compileSchema(t *testing.T, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://s.json", v); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("mem://s.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGeneratePackCommandOperations(t *testing.T) {
	snap := packCommandFixture(t, saleYAML, shiftYAML)
	mods, err := GeneratePackModules(contract.PackManifest{Name: "shop", Version: "1"}, snap)
	if err != nil {
		t.Fatal(err)
	}
	core, app := mods[0], mods[1]
	if err := contract.ValidateModuleSet(mods, func(contract.SchemaRef) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if po := core.PublishedOperations; len(po) != 2 || len(app.Requires) != 2 || !po[0].OfflineEligible || po[0].Presentation.Layout != "line_entry" || po[0].AllowedCallers[0] != "shop.app" {
		t.Fatalf("operations %+v requires %+v", po, app.Requires)
	}
	if _, err := kernel.DecodeCommandResources(snap.Commands); err != nil { // runtime decoder needs all refs
		t.Fatal(err)
	}
	schemas := map[string]*jsonschema.Schema{}
	for _, d := range core.Schemas {
		if strings.Contains(d.Ref.ID, "record_sale") || strings.Contains(d.Ref.ID, "open_shift") {
			schemas[d.Ref.ID] = compileSchema(t, d.Schema)
		}
	}
	tests := []struct {
		name, schema, payload string
		ok                    bool
	}{
		{"record_sale", "shop.core.record_sale.input", `{"register":"R1","items":[{"item":"tea","quantity":2}],"payments":[{"amount":300}]}`, true},
		{"unknown key", "shop.core.record_sale.input", `{"register":"R1","bogus":1}`, false},
		{"open_shift register only", "shop.core.open_shift.input", `{"register":"R1"}`, true},
		{"output with reference", "shop.core.record_sale.output", `{"command":"shop.record_sale","steps":[],"output":{"reference":"S-1"}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v any
			_ = json.Unmarshal([]byte(tt.payload), &v)
			if err := schemas[tt.schema].Validate(v); (err == nil) != tt.ok {
				t.Fatalf("ok=%v err=%v", tt.ok, err)
			}
		})
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	var providers []plugin.ProviderRegistration
	for _, offer := range core.Provides {
		providers = append(providers, plugin.ProviderRegistration{TenantID: scope.TenantID, ProviderID: "p-" + string(offer.Name), OwnerModuleID: core.ID, Kind: plugin.ProviderInternal, Offers: []contract.CapabilityOffer{offer},
			Factories: map[contract.CapabilityName]plugin.ProviderFactory{offer.Name: plugin.NewStaticFactory(offer.Name, nil)}})
	}
	plan, err := plugin.ResolveInstalledModules(scope, nil, mods, providers, nil, 1)
	if err != nil || len(plan.Blockers) != 0 || len(plan.Bindings) != 2 {
		t.Fatalf("plan %+v err %v", plan, err)
	}
}

func TestGeneratePackCommandRejects(t *testing.T) {
	foreign := strings.Replace(shiftYAML, "shop.core", "shop", 1)
	mixed := strings.Replace(shiftYAML, "shop.shift_commands", "shop.items", 1)
	tests := []struct{ name, yaml, want string }{
		{"owner without dot", foreign, "pack.module_owner_mismatch"},
		{"capability shared with query", mixed, "pack.capability_mixed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GeneratePackModules(contract.PackManifest{Name: "shop", Version: "1"}, packCommandFixture(t, tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %s", err, tt.want)
			}
		})
	}
	if _, err := kernel.ParseCommandResourceJSON([]byte(`{"name":"a","namespace":"b","version":1,"input":{"record":"X"},"module_owner_id":"b.core","capability":"b.c","operation":"a","transaction":[{"create":{"record":"X","values":{"f":"v"}}}]}`)); err == nil {
		t.Fatal("runtime decode accepted a command without schema refs")
	}
}

func TestPackPaymentDescriptor(t *testing.T) {
	guarded := strings.Replace(saleYAML, "transaction:", "require:\n  - payment: {for_each: $input.payments, reference: $item.reference, amount: $item.amount, currency: $input.currency}\ntransaction:", 1)
	tests := []struct {
		name, yaml, want, wantErr string
		mutate                    func(*doctype.ConfigSnapshot)
	}{
		{name: "record_sale", yaml: guarded, want: `{"source_doctype":"Sale","lines_field":"payments","reference_field":"reference","amount_field":"amount","currency_field":"currency"}`},
		{name: "unguarded has none", yaml: saleYAML, want: ""},
		{name: "unknown record", yaml: guarded, wantErr: "pack.payment_binding_invalid", mutate: func(s *doctype.ConfigSnapshot) { s.DocTypes = s.DocTypes[1:] }},
		{name: "lines not a table", yaml: strings.Replace(guarded, "$input.payments", "$input.register", 1), wantErr: "pack.payment_binding_invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := packCommandFixture(t, tt.yaml)
			if tt.mutate != nil {
				tt.mutate(snap)
			}
			mods, err := GeneratePackModules(contract.PackManifest{Name: "shop", Version: "1"}, snap)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := contract.ValidateModuleSet(mods, func(contract.SchemaRef) bool { return true }); err != nil {
				t.Fatal(err)
			}
			got := ""
			if p := mods[0].PublishedOperations[0].Payment; p != nil {
				b, _ := json.Marshal(p)
				got = string(b)
			}
			if got != tt.want {
				t.Fatalf("payment = %s, want %s", got, tt.want)
			}
		})
	}
}
