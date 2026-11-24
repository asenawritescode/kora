//go:build integration

package engine

import (
	"os"
	"strings"
	"testing"
)

// TestChatLooksUpRecordsBeforeAnswering plays a model that answers "none exist" from memory, as Zana did on
// WhatsApp after an earlier wrong answer. The reply is sent back once with a tool call required, the model looks the
// records up, and the turn ends on the answer from the result. A message that names no record type is not sent back.
func TestChatLooksUpRecordsBeforeAnswering(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_RECORD_TASK_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_RECORD_TASK_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			f := newChatFixture(t, tc.driver, dsn)
			f.useScriptedGateway()
			f.reset(seedRecordTaskBase)

			bodies, reply := f.chatScripted("List all items", []packChatStep{
				textStep("No items exist in the workspace yet."),
				callStep("find_records", map[string]any{"record_type": "Item"}),
				textStep("There are 2 items: Sugar 2kg and Salt 500g."),
			})
			if !strings.Contains(bodies[1], `"tool_choice":"required"`) || !strings.Contains(bodies[1], "answered without looking anything up") {
				t.Fatalf("the unchecked answer was not sent back with a tool call required: %s", bodies[1])
			}
			if !strings.Contains(bodies[2], `"tool_choice":"auto"`) || !strings.Contains(bodies[2], "Sugar 2kg") {
				t.Fatalf("the answer after the lookup does not see the records or cannot answer freely: %s", bodies[2])
			}
			if reply != "There are 2 items: Sugar 2kg and Salt 500g." {
				t.Fatalf("reply = %q", reply)
			}

			if _, reply = f.chatScripted("Thanks", []packChatStep{textStep("You're welcome.\n\n1. List all items\n2. Add an item")}); !strings.HasPrefix(reply, "You're welcome.") {
				t.Fatalf("reply = %q", reply)
			}

			// "1" picks "List all items" from the reply before: an answer from memory is sent back too.
			bodies, reply = f.chatScripted("1", []packChatStep{
				textStep("None found: no items have been created yet."),
				callStep("find_records", map[string]any{"record_type": "Item"}),
				textStep("There are 2 items: Sugar 2kg and Salt 500g."),
			})
			if !strings.Contains(bodies[1], `"tool_choice":"required"`) || reply != "There are 2 items: Sugar 2kg and Salt 500g." {
				t.Fatalf("the answer to a picked option was not looked up; reply %q", reply)
			}
		})
	}
}
