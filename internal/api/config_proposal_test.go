package api

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
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

func TestUpsertWorkflowDefinitionReplacesByStableID(t *testing.T) {
	snapshot := &doctype.ConfigSnapshot{WorkflowDefinitions: []contract.WorkflowDefinition{{
		ID: "record-notify", Version: 1, Entry: "finish",
		Steps: []contract.StepDefinition{{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}}},
	}}}
	updated := contract.WorkflowDefinition{
		ID: "record-notify", Version: 2, Entry: "finish",
		Steps: []contract.StepDefinition{{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "updated"}}},
	}
	upsertWorkflowDefinition(snapshot, updated)
	if len(snapshot.WorkflowDefinitions) != 1 || snapshot.WorkflowDefinitions[0].Version != 2 || snapshot.WorkflowDefinitions[0].Steps[0].End.Outcome != "updated" {
		t.Fatalf("workflow definition was not replaced by stable ID: %#v", snapshot.WorkflowDefinitions)
	}
	updated.ID = "another-workflow"
	upsertWorkflowDefinition(snapshot, updated)
	if len(snapshot.WorkflowDefinitions) != 2 {
		t.Fatalf("new workflow definition was not appended: %#v", snapshot.WorkflowDefinitions)
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

func TestConfigurationDraftLabelUsesWorkspaceContextInsteadOfProposalID(t *testing.T) {
	label := configurationDraftLabel(DraftConfigurationRequest{
		ProposalID:        "setup-opaque-123456",
		InterviewSnapshot: map[string]any{"industry": "  Neighbourhood   coffee shop  "},
	})
	if label != "Neighbourhood coffee shop setup draft" {
		t.Fatalf("draft label = %q", label)
	}
	if got := configurationDraftLabel(DraftConfigurationRequest{ProposalID: "opaque-id"}); got != "Workspace setup draft" {
		t.Fatalf("fallback label = %q", got)
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
