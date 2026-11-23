//go:build integration

package engine

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Pack actions through the chat, on the real POS pack. A scripted model reads the till actions with list_actions,
// previews them with run_action, and asks the person to confirm. After the person's yes, the next turn's request
// lists the open preview's write_id, and the model runs confirm_write with it. The chat opens a till and records a
// cash sale; the POS writes one till session and one sale. Then the same operation is sent again with the same
// idempotency key, and the POS writes nothing new.
const (
	packChatOpenTill   = "pos.core~pos.drawer~open_session"
	packChatRecordSale = "pos.core~pos.checkout~record_sale"
)

func TestPackActionsChatAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			f := newChatFixture(t, tc.driver, dsn)
			f.reset(func(f *chatFixture) {
				f.seed("Location", "LOC-1", map[string]any{"location_name": "Main"})
				f.seed("Register", "REG-1", map[string]any{"register_name": "Front", "location": "LOC-1", "enabled": true})
				f.seed("Item", "ITM-1", map[string]any{"item_name": "Maize", "uom": "Nos", "price": 180, "cost": 120})
				f.seed("Stock Balance", "SB-1", map[string]any{"item": "ITM-1", "location": "LOC-1", "quantity": 20, "reorder_level": 4})
			})
			f.useScriptedGateway()
			saleIntent := f.cashIntent("pack-chat-intent-1", 180)
			f.thread = "pack-chat-" + tc.driver

			// Turn 1: the model lists the till actions, previews the opening with run_action, and asks.
			f.chatScripted("Open my till with 500 float at Front.", []packChatStep{
				{tool: "list_actions", args: func() string { return `{"record_type":"Till Session"}` }},
				{tool: "run_action", args: func() string {
					return fmt.Sprintf(`{"action":%q,"input":{"register":"REG-1","location":"LOC-1","opening_float":500}}`, packChatOpenTill)
				}},
				{content: "Preview ready."},
			})
			if n := f.count("Till Session"); n != "0" {
				t.Fatalf("till sessions before confirmation = %s, want 0", n)
			}
			// Turn 2: after yes, the model confirms the open preview by its write_id, and the till opens.
			// The write_id is read from this turn's request body, where the open previews are listed.
			var tillWriteID string
			f.chatScripted("yes", []packChatStep{
				{tool: "confirm_write", args: func() string {
					tillWriteID = writeIDIn(f.model.lastBody())
					return fmt.Sprintf(`{"write_id":%q}`, tillWriteID)
				}},
				{content: "Your till is open."},
			})
			if tillWriteID == "" {
				t.Fatal("the turn-2 request did not list the till preview's write_id")
			}
			if n := f.count("Till Session"); n != "1" {
				t.Fatalf("till sessions after confirm_write = %s, want 1", n)
			}

			// Turn 3: the model previews the sale with run_action; the preview ends the turn before it writes.
			saleInput := func() string {
				till := f.one("SELECT name FROM " + f.tbl("Till Session"))
				return fmt.Sprintf(`{"action":%q,"input":{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":%q}]}}`, packChatRecordSale, till, saleIntent)
			}
			f.chatScripted("Sell one Maize, paid in cash.", []packChatStep{
				{tool: "list_actions", args: func() string { return fmt.Sprintf(`{"action":%q}`, packChatRecordSale) }},
				{tool: "run_action", args: saleInput},
				{content: "Preview ready."},
			})
			if n := f.count("Sale"); n != "0" {
				t.Fatalf("sales before confirmation = %s, want 0", n)
			}
			// Turn 4: after yes, the model confirms the sale preview by its write_id.
			var saleWriteID string
			f.chatScripted("yes", []packChatStep{
				{tool: "confirm_write", args: func() string {
					saleWriteID = writeIDIn(f.model.lastBody())
					return fmt.Sprintf(`{"write_id":%q}`, saleWriteID)
				}},
				{content: "Sale recorded."},
			})
			if saleWriteID == "" || saleWriteID == tillWriteID {
				t.Fatalf("the turn-4 request listed write_id %q, want the sale preview's own id", saleWriteID)
			}
			if n := f.count("Sale"); n != "1" {
				t.Fatalf("sales after confirm_write = %s, want 1", n)
			}
			// Each provider call stored its usage. The scripted model reports 10 input, 10 output and 20 total tokens.
			tokens := f.one("SELECT tokens FROM _kora_ai_usage WHERE site = ? AND status = ? ORDER BY occurred_at DESC", f.siteName, "completed")
			t.Logf("stored usage tokens: %s", tokens)
			if !strings.Contains(tokens, `"total":20`) {
				t.Fatalf("stored usage tokens = %s, want the scripted total of 20", tokens)
			}
			// The provider was offered the two pack-action tools and the run answered "completed".
			if first := f.model.bodies[0]; !strings.Contains(first, `"name":"run_action"`) || !strings.Contains(first, `"name":"list_actions"`) || !strings.Contains(first, `"name":"confirm_write"`) {
				t.Fatalf("pack-action tools were not offered: %.300s", first)
			}
			if last := f.model.bodies[len(f.model.bodies)-1]; !strings.Contains(last, `\"status\":\"completed\"`) {
				t.Fatalf("the sale result did not reach the model as completed: %.600s", last)
			}

			// Replay: the same operation with the same key, sent again, writes nothing and returns the first receipt.
			replayBody := func(intent string) string {
				till := f.one("SELECT name FROM " + f.tbl("Till Session"))
				return fmt.Sprintf(`{"meta":{"idempotency_key":"pack-chat-replay"},"input_schema":{"id":"pos.core.record_sale.input","version":1},"input":{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":1,"unit_price":180,"amount":180}],"payments":[{"method":"Cash","amount":180,"currency":"KES","reference":%q}]}}`, till, intent)
			}
			replayIntent := f.cashIntent("pack-chat-intent-2", 180)
			post := func() (int, string) {
				return f.postOperation(packChatRecordSale, replayBody(replayIntent))
			}
			code, first := post()
			if code != http.StatusOK || first == "" {
				t.Fatalf("first replay send = %d", code)
			}
			code, second := post()
			if code != http.StatusOK || second != first {
				t.Fatalf("replay = %d execution %q, want %q", code, second, first)
			}
			if n := f.count("Sale"); n != "2" {
				t.Fatalf("sales after replay = %s, want 2 (one from the chat, one from the replayed key)", n)
			}
		})
	}
}
