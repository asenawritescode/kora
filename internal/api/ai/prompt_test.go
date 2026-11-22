package ai

import (
	"context"
	"encoding/json"
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

func TestTurnContextMessageJoinsNonEmptyParts(t *testing.T) {
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "no parts", parts: nil, want: ""},
		{name: "only blank parts", parts: []string{"", "  \n\n"}, want: ""},
		{name: "one part is trimmed", parts: []string{"\n\nCURRENT PAGE CONTEXT:\n- pathname: /x\n"}, want: "CURRENT PAGE CONTEXT:\n- pathname: /x"},
		{name: "parts keep their order", parts: []string{"\n\nA", "", "\n\nB "}, want: "A\n\nB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := turnContextMessage(tc.parts...); got != tc.want {
				t.Errorf("turnContextMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildChatMessagesPlacesTurnContextBeforeTheUserMessage(t *testing.T) {
	history := []ChatMessage{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "reply"}}
	tests := []struct {
		name        string
		turnContext string
		wantRoles   []string
	}{
		{name: "no turn context", turnContext: "", wantRoles: []string{"system", "user", "assistant", "user"}},
		{name: "turn context", turnContext: "CURRENT PAGE CONTEXT", wantRoles: []string{"system", "user", "assistant", "system", "user"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			messages := buildChatMessages("standing", history, tc.turnContext, "now")
			var roles []string
			for _, m := range messages {
				roles = append(roles, m["role"].(string))
			}
			if strings.Join(roles, ",") != strings.Join(tc.wantRoles, ",") {
				t.Fatalf("roles = %v, want %v", roles, tc.wantRoles)
			}
			if messages[len(messages)-1]["content"] != "now" {
				t.Errorf("last message is not the user's message: %v", messages[len(messages)-1])
			}
		})
	}
}

// requestPrefix is the part of a provider request that prompt caching reuses: the tools and the standing system text,
// which lead every request. The turn context and the user's message come after them.
func requestPrefix(t *testing.T, messages []map[string]any, tools []map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"tools": tools, "system": messages[0]})
	if err != nil {
		t.Fatalf("marshal prefix: %v", err)
	}
	return string(raw)
}

func TestRequestPrefixIsStableAcrossPages(t *testing.T) {
	tools := buildOpenAIToolsFromCatalog(BuildToolCatalog(recordToolsRegistry(), CatalogOptions{Operations: noOperations{}}))
	standing := chatSystemPrompt + financialDisplayContext("KES") + insightsNoteFor(true)
	history := []ChatMessage{{Role: "user", Content: "earlier question"}, {Role: "assistant", Content: "earlier answer"}}
	pages := []ChatContext{
		{Pathname: "/s/demo/workspace/tasks", ShellMode: "workspace"},
		{Pathname: "/s/demo/workspace/tasks/T-1", ShellMode: "workspace", Doctype: "Task", DocumentName: "T-1"},
		{},
	}
	first := requestPrefix(t, buildChatMessages(standing, history, turnContextMessage(pageContextBlurb(pages[0])), "hi"), tools)
	for _, page := range pages[1:] {
		got := requestPrefix(t, buildChatMessages(standing, history, turnContextMessage(pageContextBlurb(page)), "hi"), tools)
		if got != first {
			t.Errorf("request prefix changed for page %q", page.Pathname)
		}
	}
	withPage := buildChatMessages(standing, history, turnContextMessage(pageContextBlurb(pages[1])), "hi")
	if !strings.Contains(withPage[len(withPage)-2]["content"].(string), "T-1") {
		t.Error("page context is not the message just before the user's message")
	}
	if strings.Contains(standing, "T-1") || strings.Contains(standing, "CURRENT PAGE CONTEXT") {
		t.Error("standing text carries page context")
	}
}
