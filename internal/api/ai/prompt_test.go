package ai

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
)

func TestChatSystemPromptIsShortAndBranded(t *testing.T) {
	prompt := chatSystemPrompt
	const maxChars = 2500
	if n := len([]rune(prompt)); n > maxChars {
		t.Errorf("system prompt is %d characters, want at most %d", n, maxChars)
	}
	if !strings.Contains(prompt, "Zana") {
		t.Error("system prompt does not name Zana")
	}
	if strings.Contains(prompt, "Kigae") {
		t.Error("system prompt still names the old brand")
	}
}

// promptVerticalWords are business words of one vertical. The Engine prompt stays generic.
var promptVerticalWords = regexp.MustCompile(`(?i)\b(sales?|tills?|pos|shops?|items?|invoices?|customers?|orders?|stock|shifts?|cash|products?|receipts?|inventory)\b`)

func TestChatSystemPromptHasNoVerticalWords(t *testing.T) {
	if hit := promptVerticalWords.FindString(chatSystemPrompt); hit != "" {
		t.Errorf("system prompt names a vertical word %q", hit)
	}
}

// promptSnakeName matches a snake_case word, the form tool names and codes take.
var promptSnakeName = regexp.MustCompile(`\b[a-z]+(?:_[a-z]+)+\b`)

// staleToolName matches the per-doctype tool names H1a removed, such as item_find or sale_delete.
var staleToolName = regexp.MustCompile(`\b[a-z]+_(find|list|get|create|update|delete)\b`)

// promptCodes are snake_case words the prompt uses that are not tool names: the reply codes and the write id.
var promptCodes = map[string]bool{"write_id": true, "missing_fields": true}

func TestChatSystemPromptNamesOnlyCurrentTools(t *testing.T) {
	prompt := chatSystemPrompt
	if hit := staleToolName.FindString(prompt); hit != "" && hit != "script_create" {
		t.Errorf("system prompt names a removed tool %q", hit)
	}
	catalog := BuildToolCatalog(recordToolsRegistry(), CatalogOptions{Operations: noOperations{}})
	known := map[string]bool{}
	for _, tool := range catalog.Tools {
		known[tool.Name] = true
	}
	for _, word := range promptSnakeName.FindAllString(prompt, -1) {
		if promptCodes[word] || known[word] {
			continue
		}
		t.Errorf("system prompt names %q, which is not a tool in the catalog", word)
	}
}

// noOperations is a pack catalog with nothing in it, enough to offer the pack-action tools.
type noOperations struct{}

func (noOperations) Catalog(policy.Execution, contract.BusinessScope) (contract.ApplicationCatalog, error) {
	return contract.ApplicationCatalog{}, nil
}

func (noOperations) Run(context.Context, policy.Execution, contract.BusinessScope, string, contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	return contract.OperationReceipt{}, nil
}

func (noOperations) Preview(context.Context, policy.Execution, contract.BusinessScope, string, contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	return contract.OperationReceipt{}, nil
}
