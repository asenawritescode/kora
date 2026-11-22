package ai

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
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

	catalog := BuildToolCatalog(reg, CatalogOptions{})
	contractCat := ToContractCatalog(catalog)

	if len(contractCat.Tools) != len(catalog.Tools) {
		t.Fatalf("contract tool count = %d, want %d", len(contractCat.Tools), len(catalog.Tools))
	}

	create := findTool(t, catalog.Tools, "create_record")
	contractCreate := findContractTool(t, contractCat.Tools, "create_record")

	if contractCreate.Operation != "create" {
		t.Errorf("contract create op = %q, want create", contractCreate.Operation)
	}
	if contractCreate.RequiresConfirmation || contractCreate.RequiresRecentAuth || contractCreate.SafetyLevel != "guarded" {
		t.Errorf("contract create should use explicit user intent with guarded kernel authorization: %+v", contractCreate)
	}
	contractUpdate := findContractTool(t, contractCat.Tools, "update_record")
	if contractUpdate.Operation != "update" || contractUpdate.RequiresConfirmation || contractUpdate.RequiresRecentAuth {
		t.Errorf("contract update should use explicit user intent with guarded kernel authorization: %+v", contractUpdate)
	}
	contractBundle := findContractTool(t, contractCat.Tools, "mutate_records")
	if !contractBundle.RequiresConfirmation || contractBundle.Operation != "system" {
		t.Errorf("related-record mutation must preserve its explicit approval requirement in the shared contract: %+v", contractBundle)
	}
	for _, channel := range contractBundle.ChannelAllowlist {
		if channel == "whatsapp" {
			t.Errorf("durably approved mutation bundle must not use header-only WhatsApp confirmation: %+v", contractBundle.ChannelAllowlist)
		}
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

	catalog := BuildToolCatalog(reg, CatalogOptions{})
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

func TestMetricsToolsAreExposedAndRetiredAnalyticsAreNot(t *testing.T) {
	catalog := BuildToolCatalog(doctype.NewRegistry(), CatalogOptions{})
	for _, name := range []string{toolListMetrics, toolQueryMetrics, toolDrillMetrics, toolAskChoice} {
		tool := findTool(t, catalog.Tools, name)
		if tool.SafetyLevel != "safe" {
			t.Errorf("%s safety = %q, want safe", name, tool.SafetyLevel)
		}
	}
	for _, name := range []string{"get_analytics_insights", "get_analytics_catalog", "query_analytics", "list_analytics_reports", "run_analytics_report"} {
		for _, tool := range catalog.Tools {
			if tool.Name == name {
				t.Errorf("retired analytics tool %s is still exposed", name)
			}
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
