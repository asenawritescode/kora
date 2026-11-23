//go:build integration

package engine

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// A cash sale through run_action names no payment reference. The preview requests no payment intent and writes
// no sale; it lists the payment it will settle. After the person's yes, confirm_write requests one cash intent and
// records the sale with that intent's id. A replayed confirm returns the first outcome and writes nothing more.
// A method whose provider waits for the customer is refused at the preview, with nothing stored.
func TestPackActionsChatSettlesCashAtConfirm(t *testing.T) {
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
			f.reset(seedRecordTaskBase)
			f.useScriptedGateway()
			f.thread = "pack-cash-" + tc.driver
			sale := fmt.Sprintf(`{"action":%q,"input":{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":2,"unit_price":250,"amount":500}],"payments":[{"method":"Cash","amount":500,"currency":"KES"}]}}`, recordTaskRecordSale, recordTaskTill)

			// Turn 1: the preview names the payment and writes nothing: no intent, no sale. The preview ends the turn,
			// so the payment line is read from the stored preview.
			f.chatScripted("Sell 2 Sugar 2kg, cash.", []packChatStep{
				{tool: "run_action", args: func() string { return sale }},
				{content: "Preview ready."},
			})
			if !strings.Contains(f.one("SELECT preview_json FROM _kora_ai_pending_write WHERE site = ? AND state = ?", f.siteName, "previewed"), "Cash payment of 500 KES") {
				t.Fatal("the stored preview does not name the cash payment")
			}
			if n := f.one("SELECT COUNT(*) FROM _kora_payment_intent WHERE site = ?", f.siteName); n != "0" {
				t.Fatalf("payment intents after preview = %s, want 0", n)
			}
			if n := f.count("Sale"); n != "0" {
				t.Fatalf("sales after preview = %s, want 0", n)
			}

			// Turn 2: after yes, confirm_write settles the payment and records the sale.
			var writeID string
			f.chatScripted("yes", []packChatStep{
				{tool: "confirm_write", args: func() string {
					writeID = writeIDIn(f.model.lastBody())
					return fmt.Sprintf(`{"write_id":%q}`, writeID)
				}},
				{content: "Sold 2 Sugar 2kg for 500 KES in cash."},
			})
			if writeID == "" {
				t.Fatal("the turn-2 request did not list the sale preview's write_id")
			}
			if n := f.one("SELECT COUNT(*) FROM _kora_payment_intent WHERE site = ? AND state = ? AND consumed_by <> ''", f.siteName, "confirmed"); n != "1" {
				t.Fatalf("confirmed consumed intents after confirm = %s, want 1", n)
			}
			intent := f.one("SELECT id FROM _kora_payment_intent WHERE site = ?", f.siteName)
			if n := f.count("Sale"); n != "1" {
				t.Fatalf("sales after confirm_write = %s, want 1", n)
			}
			if ref := f.one("SELECT reference FROM " + f.dialect.QuoteIdent("tabSale__payments")); ref != intent {
				t.Fatalf("sale payment reference = %q, want the intent %q", ref, intent)
			}

			// Replay: the same confirm again returns the first outcome; no second intent and no second sale.
			f.chatScripted("yes", []packChatStep{
				{tool: "confirm_write", args: func() string { return fmt.Sprintf(`{"write_id":%q}`, writeID) }},
				{content: "Sold 2 Sugar 2kg for 500 KES in cash."},
			})
			if n := f.one("SELECT COUNT(*) FROM _kora_payment_intent WHERE site = ?", f.siteName); n != "1" {
				t.Fatalf("payment intents after replay = %s, want 1", n)
			}
			if n := f.count("Sale"); n != "1" {
				t.Fatalf("sales after replay = %s, want 1", n)
			}
		})
	}
}

// TestPackActionsChatRefusesCustomerPaymentAtPreview checks that a method whose payment waits for the customer
// (here Mobile Money, which has no provider that confirms on request) is refused at the preview with a structured
// error, and that the refused preview stores nothing.
func TestPackActionsChatRefusesCustomerPaymentAtPreview(t *testing.T) {
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
			f.reset(seedRecordTaskBase)
			f.useScriptedGateway()
			f.thread = "pack-needs-till-" + tc.driver
			sale := fmt.Sprintf(`{"action":%q,"input":{"register":"REG-1","location":"LOC-1","till_session":%q,"currency":"KES","items":[{"item":"ITM-1","quantity":2,"unit_price":250,"amount":500}],"payments":[{"method":"Mobile Money","amount":500,"currency":"KES"}]}}`, recordTaskRecordSale, recordTaskTill)
			bodies, _ := f.chatScripted("Sell 2 Sugar 2kg, paid by mobile money.", []packChatStep{
				{tool: "run_action", args: func() string { return sale }},
				{content: "Mobile money needs the till. Nothing was saved."},
			})
			if last := bodies[len(bodies)-1]; !strings.Contains(last, `\"code\":\"guard_failed\"`) || !strings.Contains(last, "this payment method needs the till") {
				t.Fatalf("the preview was not refused as needing the till: %.400s", last)
			}
			if f.openPreviews() != 0 {
				t.Fatal("a refused preview left an open write")
			}
			if n := f.one("SELECT COUNT(*) FROM _kora_payment_intent WHERE site = ?", f.siteName); n != "0" {
				t.Fatalf("payment intents after refusal = %s, want 0", n)
			}
		})
	}
}
