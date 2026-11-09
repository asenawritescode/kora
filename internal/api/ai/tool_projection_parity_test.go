package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestToolProjectionParityAcrossAdapters(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name: "Customer",
		Fields: []doctype.Field{
			{Fieldname: "name", Fieldtype: "Data", Label: "Name", Reqd: true},
			{Fieldname: "email", Fieldtype: "Data", Label: "Email"},
		},
	})

	catalog := BuildToolCatalog(reg)
	contractCatalog := ToContractCatalog(catalog)
	openAITools := buildOpenAIToolsFromCatalog(catalog)
	channelVersion := contract.ToolCatalog{Version: contractCatalog.Version, Tools: contractCatalog.Tools}

	if len(contractCatalog.Tools) != len(catalog.Tools) {
		t.Fatalf("contract catalog count = %d, want %d", len(contractCatalog.Tools), len(catalog.Tools))
	}
	if len(openAITools) != len(catalog.Tools) {
		t.Fatalf("openai tool count = %d, want %d", len(openAITools), len(catalog.Tools))
	}

	create := findTool(t, catalog.Tools, "customer_create")
	contractCreate := findContractTool(t, contractCatalog.Tools, "customer_create")
	if contractCreate.Name != create.Name || contractCreate.Operation != create.Operation || contractCreate.Doctype != create.Doctype {
		t.Fatalf("contract projection lost identity: got %+v, want %+v", contractCreate, create)
	}

	if !containsOpenAITool(openAITools, "customer_create") {
		t.Fatal("openai projection missing customer_create")
	}

	if _, err := json.Marshal(contractCatalog); err != nil {
		t.Fatalf("contract catalog marshal failed: %v", err)
	}
	if _, err := json.Marshal(channelVersion); err != nil {
		t.Fatalf("channel projection marshal failed: %v", err)
	}

	// The public wire shape must stay free of internal empty-string drift.
	wire, err := json.Marshal(contractCatalog)
	if err != nil {
		t.Fatalf("marshal contract catalog: %v", err)
	}
	if !strings.Contains(string(wire), `"customer_create"`) {
		t.Fatalf("contract catalog missing tool name in JSON: %s", wire)
	}
}

func containsOpenAITool(tools []map[string]any, name string) bool {
	for _, raw := range tools {
		fn, _ := raw["function"].(map[string]any)
		if fn["name"] == name {
			return true
		}
	}
	return false
}
