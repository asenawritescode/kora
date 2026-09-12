package api

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestBuildSystemGraphUsesDocTypesAsCanonicalNodes(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{
		{Name: "Supplier", Module: "Procurement"},
		{Name: "Purchase Order", Module: "Procurement", Fields: []doctype.Field{{Fieldname: "supplier", Fieldtype: "Link", Options: "Supplier"}}},
		{Name: "Purchase Item", IsChildTable: true},
	}, nil, nil)
	result := BuildSystemGraph(reg, []*doctype.Workflow{{Name: "Purchase approval", DocumentType: "Purchase Order", IsActive: true, States: []doctype.WorkflowState{{State: "Draft"}}, Transitions: []doctype.WorkflowTransition{{Action: "approve", From: "Draft", To: "Approved", Allowed: "Manager"}}}})

	for _, node := range result.Nodes {
		if node.Kind == "entity" {
			t.Fatal("graph must not expose Entity as a runtime node kind")
		}
	}
	if len(result.Nodes) != 4 { // 2 DocTypes, 1 workflow, 1 transition inspection node
		t.Fatalf("nodes = %d, want 4", len(result.Nodes))
	}
	if len(result.Edges) != 3 {
		t.Fatalf("edges = %d, want 3", len(result.Edges))
	}
	if result.Edges[0].Relation != "links to" {
		t.Fatalf("first edge = %#v, want link projection", result.Edges[0])
	}
}

func TestBuildSystemGraphIsEmptyForNilRegistry(t *testing.T) {
	result := BuildSystemGraph(nil, nil)
	if result.Nodes == nil || result.Edges == nil {
		t.Fatal("empty graph must serialize as empty arrays")
	}
}
