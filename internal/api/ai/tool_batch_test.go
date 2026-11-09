package ai

import (
	"context"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

func TestExecuteToolCallsForAIRejectsMalformedCalls(t *testing.T) {
	valid := testAIExecution(t, "site-a", "alice", []string{doctype.AdminRole})
	tests := []struct {
		name string
		call any
		want string
	}{
		{"not an object", "junk", "invalid tool call format"},
		{"missing function", map[string]any{"id": "c1"}, "missing function in tool call"},
		{"bad arguments", map[string]any{"id": "c1", "function": map[string]any{"name": "list_doctypes", "arguments": "{"}}, "invalid arguments JSON"},
		{"known tool runs", map[string]any{"id": "c1", "function": map[string]any{"name": "list_doctypes", "arguments": "{}"}}, "No doctypes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := &orm.TxManager{Context: context.Background(), Dialect: db.Resolve("mysql"), Execution: valid}
			results := executeToolCallsForAI(context.Background(), tx, doctype.NewRegistry(), toolCallBatch{
				Calls: []any{tt.call}, Owner: "forged", Site: "site-a", RunID: "run-1", StepID: "step-1", ConversationID: "conv-1",
			})
			if len(results) != 1 {
				t.Fatalf("results = %v", results)
			}
			if content, _ := results[0]["content"].(string); !strings.Contains(content, tt.want) {
				t.Fatalf("content = %q, want it to contain %q", content, tt.want)
			}
		})
	}
}
