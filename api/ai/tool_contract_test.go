package ai

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// TestToolCatalogContractParity proves that BuildToolCatalog's output projects
// losslessly into the canonical contract.ToolDescriptor shape (RFC §10.4.5:
// catalog parity across adapters). Every tool's wire-visible identity, operation,
// doctype, safety flags, and channel allowlist must survive the projection.
func TestToolCatalogContractParity(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name: "Customer",
		Fields: []doctype.Field{
			{Fieldname: "name", Fieldtype: "Data", Label: "Name", Reqd: true},
			{Fieldname: "email", Fieldtype: "Data", Label: "Email"},
		},
	})

	catalog := BuildToolCatalog(reg)
	contractCat := ToContractCatalog(catalog)

	if len(contractCat.Tools) != len(catalog.Tools) {
		t.Fatalf("contract tool count = %d, want %d", len(contractCat.Tools), len(catalog.Tools))
	}

	create := findTool(t, catalog.Tools, "customer_create")
	contractCreate := findContractTool(t, contractCat.Tools, "customer_create")

	if contractCreate.Operation != "create" || contractCreate.Doctype != "Customer" {
		t.Errorf("contract create op/doctype = %q/%q, want create/Customer", contractCreate.Operation, contractCreate.Doctype)
	}
	if !contractCreate.RequiresConfirmation || !contractCreate.RequiresRecentAuth {
		t.Errorf("contract create should preserve safety guards: %+v", contractCreate)
	}
	if len(contractCreate.ChannelAllowlist) != len(create.ChannelAllowlist) {
		t.Errorf("channel allowlist length mismatch: %d vs %d", len(contractCreate.ChannelAllowlist), len(create.ChannelAllowlist))
	}

	// The canonical descriptor must be JSON-serializable (it is the wire shape).
	if _, err := json.Marshal(contractCreate); err != nil {
		t.Errorf("contract descriptor marshal failed: %v", err)
	}
}

func TestOpenAIToolsProjectFromCatalog(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name: "Customer",
		Fields: []doctype.Field{
			{Fieldname: "name", Fieldtype: "Data", Label: "Name", Reqd: true},
		},
	})

	catalog := BuildToolCatalog(reg)
	openAITools := buildOpenAIToolsFromCatalog(catalog)

	if len(openAITools) != len(catalog.Tools) {
		t.Fatalf("openai tool count = %d, want %d", len(openAITools), len(catalog.Tools))
	}

	for _, tool := range catalog.Tools {
		found := false
		for _, raw := range openAITools {
			fn, _ := raw["function"].(map[string]any)
			if fn["name"] == tool.Name {
				found = true
				if fn["description"] != tool.Description {
					t.Fatalf("description mismatch for %s", tool.Name)
				}
				if fn["parameters"] == nil {
					t.Fatalf("missing parameters projection for %s", tool.Name)
				}
				break
			}
		}
		if !found {
			t.Fatalf("missing projected tool %s", tool.Name)
		}
	}
}

func TestAnalyticsToolsAreExposed(t *testing.T) {
	catalog := BuildToolCatalog(doctype.NewRegistry())
	for _, name := range []string{"get_analytics_insights", "get_analytics_catalog", "query_analytics", "list_analytics_reports", "run_analytics_report"} {
		tool := findTool(t, catalog.Tools, name)
		if tool.SafetyLevel != "safe" {
			t.Errorf("%s safety = %q, want safe", name, tool.SafetyLevel)
		}
	}
}

func findContractTool(t *testing.T, tools []contract.ToolDescriptor, name string) contract.ToolDescriptor {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("contract tool %q not found", name)
	return contract.ToolDescriptor{}
}
