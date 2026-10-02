package api

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestOrderDraftDocTypesChildBeforeParent(t *testing.T) {
	parent := &doctype.DocType{Name: "Purchase Request", Fields: []doctype.Field{{Fieldname: "items", Fieldtype: "Table", Options: "Purchase Request Item"}}}
	child := &doctype.DocType{Name: "Purchase Request Item", IsChildTable: true}
	ordered, err := orderDraftDocTypes([]*doctype.DocType{parent, child})
	if err != nil {
		t.Fatal(err)
	}
	if ordered[0].Name != child.Name || ordered[1].Name != parent.Name {
		t.Fatalf("order = %v, want child before parent", orderedDocTypeNames(ordered))
	}
}

func TestOrderDraftDocTypesRejectsNormalizedDuplicate(t *testing.T) {
	_, err := orderDraftDocTypes([]*doctype.DocType{{Name: "Supplier"}, {Name: " supplier "}})
	if err == nil {
		t.Fatal("expected normalized name conflict")
	}
}

func TestOrderDraftDocTypesRejectsCycle(t *testing.T) {
	a := &doctype.DocType{Name: "A", Fields: []doctype.Field{{Fieldname: "b", Fieldtype: "Link", Options: "B"}}}
	b := &doctype.DocType{Name: "B", Fields: []doctype.Field{{Fieldname: "a", Fieldtype: "Link", Options: "A"}}}
	if _, err := orderDraftDocTypes([]*doctype.DocType{a, b}); err == nil {
		t.Fatal("expected dependency cycle")
	}
}

func TestClassifyCandidateDocTypeReusesIdenticalLiveDefinition(t *testing.T) {
	reg := doctype.NewRegistry()
	existing := &doctype.DocType{Name: "Table", Module: "Restaurant", Fields: []doctype.Field{{Fieldname: "seats", Fieldtype: "Int"}}}
	reg.Register(existing)

	action, safe, conflict := classifyCandidateDocType(reg, &doctype.DocType{Name: "Table", Module: "Restaurant", Fields: []doctype.Field{{Fieldname: "seats", Fieldtype: "Int"}}})
	if action != "reuse" || !safe || conflict != nil {
		t.Fatalf("classification = action %q safe %v conflict %#v, want reuse/true/nil", action, safe, conflict)
	}
}

func TestClassifyCandidateDocTypeBlocksChangedLiveDefinition(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Table", Module: "Restaurant", Fields: []doctype.Field{{Fieldname: "seats", Fieldtype: "Int"}}})

	action, safe, conflict := classifyCandidateDocType(reg, &doctype.DocType{Name: "Table", Module: "Restaurant", Fields: []doctype.Field{{Fieldname: "status", Fieldtype: "Select", Options: "Available\nSeated"}}})
	if action != "update" || conflict == nil {
		t.Fatalf("classification = action %q safe %v conflict %#v, want update with conflict", action, safe, conflict)
	}
}

func TestValidateScriptCandidatesRequiresSafeAPIContract(t *testing.T) {
	valid := &ScriptCandidate{Name: "send_invoice", ScriptType: "api_method", MethodPath: "/send-invoice", Source: "return { success: true };"}
	if err := validateScriptCandidates(&Handler{}, []*ScriptCandidate{valid}); err != nil {
		t.Fatalf("valid script rejected: %v", err)
	}
	cases := []*ScriptCandidate{
		{Name: "missing-source", ScriptType: "api_method", MethodPath: "/send"},
		{Name: "missing-path", ScriptType: "api_method", Source: "return {};"},
		{Name: "unsafe", ScriptType: "api_method", MethodPath: "/unsafe", Source: "const api_key = 'hidden';"},
	}
	for _, candidate := range cases {
		if err := validateScriptCandidates(&Handler{}, []*ScriptCandidate{candidate}); err == nil {
			t.Errorf("expected script %q to be rejected", candidate.Name)
		}
	}
}
