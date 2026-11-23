//go:build integration || liveeval

package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The ten record tasks of the H0 baseline, on the POS pack with the seed in seedRecordTaskBase. The scripted evals
// (TestRecordTaskEvals, CI) and the live evals (TestLiveRecordTaskEvals, planner) share these specs: the request
// text, the SQL state a finished task must leave, and the reply rule of the read and refusal tasks.

// recordTask is one eval task.
type recordTask struct {
	id      int
	message string
	// writes marks a task that previews a write and waits for the person's yes.
	writes bool
	// settled checks the SQL state after the task is finished (after the yes turn for a write).
	settled func(f *chatFixture) error
	// answered checks the final reply. It is nil when the task has no reply rule.
	answered func(reply string) bool
}

// scriptedRecordTurn is one scripted chat turn: the person's message, the model's answers and a check of what the
// model saw during the turn.
type scriptedRecordTurn struct {
	message string
	steps   func(f *chatFixture) []packChatStep
	check   func(f *chatFixture, bodies []string) error
}

const (
	recordTaskTill       = "TILL-1"
	recordTaskPayOut     = "pos.core~pos.drawer~record_pay_out"
	recordTaskRecordSale = "pos.core~pos.checkout~record_sale"
)

// seedRecordTaskBase is the seed of every record task: a location, a register, two items with stock, and an open
// till for the signed-in cashier.
func seedRecordTaskBase(f *chatFixture) {
	f.seed("Location", "LOC-1", map[string]any{"location_name": "Main"})
	f.seed("Register", "REG-1", map[string]any{"register_name": "Front", "location": "LOC-1", "enabled": true})
	f.seed("Item", "ITM-1", map[string]any{"item_name": "Sugar 2kg", "uom": "Nos", "price": 250, "cost": 150})
	f.seed("Item", "ITM-2", map[string]any{"item_name": "Salt 500g", "uom": "Nos", "price": 80, "cost": 50})
	f.seed("Stock Balance", "SB-1", map[string]any{"item": "ITM-1", "location": "LOC-1", "quantity": 20, "reorder_level": 4})
	f.seed("Shift", "SHIFT-1", map[string]any{"register": "REG-1", "cashier": packChatUser, "status": "Open"})
	f.seed("Till Session", recordTaskTill, map[string]any{
		"shift": "SHIFT-1", "register": "REG-1", "location": "LOC-1", "cashier": packChatUser,
		"opening_float": 500, "expected_cash": 500, "status": "Open",
	})
}

// recordTasks are the ten tasks in H0 order.
var recordTasks = []recordTask{
	{id: 1, message: "Add product Sugar 1kg, price 140.", writes: true,
		settled: func(f *chatFixture) error {
			return expectRows(f, "Item", 3, "item_name = ?", "Sugar 1kg", "price", 140)
		}},
	{id: 2, message: "Add customer Jane Wanjiru, 0712345678.",
		settled: func(f *chatFixture) error { return expectCountOf(f, "Item", 2) },
		answered: func(reply string) bool {
			low := strings.ToLower(reply)
			return strings.Contains(low, "customer") && (strings.Contains(low, "no ") || strings.Contains(low, "not ") || strings.Contains(low, "cannot"))
		}},
	{id: 3, message: "Change the price of Sugar 2kg to 260.", writes: true,
		settled: func(f *chatFixture) error {
			return expectRows(f, "Item", 2, "item_name = ?", "Sugar 2kg", "price", 260)
		}},
	{id: 4, message: "Cash pay-out 500 for transport.", writes: true,
		settled: func(f *chatFixture) error {
			if err := expectCountOf(f, "Cash Movement", 1); err != nil {
				return err
			}
			if err := expectNumber(f, "SELECT amount FROM "+f.tbl("Cash Movement"), 500); err != nil {
				return err
			}
			return expectNumber(f, "SELECT expected_cash FROM "+f.tbl("Till Session")+" WHERE name = ?", 0, recordTaskTill)
		}},
	{id: 5, message: "Which sale had the most items today?",
		settled: func(f *chatFixture) error { return expectCountOf(f, "Sale", 0) },
		answered: func(reply string) bool {
			low := strings.ToLower(reply)
			return (strings.Contains(low, "cannot") || strings.Contains(low, "can't")) && !strings.ContainsAny(reply, "0123456789")
		}},
	{id: 6, message: "How many products do we have?",
		settled:  func(f *chatFixture) error { return expectCountOf(f, "Item", 2) },
		answered: func(reply string) bool { return strings.Contains(reply, "2") }},
	{id: 7, message: "Add 3 products: Tea 50, Milk 60, Bread 55.", writes: true,
		settled: func(f *chatFixture) error {
			if err := expectCountOf(f, "Item", 5); err != nil {
				return err
			}
			for name, price := range map[string]float64{"Tea": 50, "Milk": 60, "Bread": 55} {
				if err := expectRows(f, "Item", 5, "item_name = ?", name, "price", price); err != nil {
					return err
				}
			}
			return nil
		}},
	{id: 8, message: "Add a product.",
		settled: func(f *chatFixture) error { return expectCountOf(f, "Item", 2) },
		answered: func(reply string) bool {
			low := strings.ToLower(reply)
			return strings.Contains(low, "name") && strings.Contains(low, "price")
		}},
	{id: 9, message: "Delete product Salt 500g.",
		settled: func(f *chatFixture) error {
			if err := expectCountOf(f, "Item", 2); err != nil {
				return err
			}
			return expectRows(f, "Item", 2, "item_name = ?", "Salt 500g", "price", 80)
		},
		answered: func(reply string) bool {
			low := strings.ToLower(reply)
			return strings.Contains(low, "delete") && (strings.Contains(low, "not available") || strings.Contains(low, "cannot") || strings.Contains(low, "can't") || strings.Contains(low, "continue"))
		}},
	{id: 10, message: "Sell 2 Sugar 2kg, cash.", writes: true,
		settled: func(f *chatFixture) error {
			if err := expectCountOf(f, "Sale", 1); err != nil {
				return err
			}
			// The cash payment was requested at confirm, once, and the sale holds its id.
			if n := f.one("SELECT COUNT(*) FROM _kora_payment_intent WHERE site = ?", f.siteName); n != "1" {
				return fmt.Errorf("payment intents = %s, want 1", n)
			}
			return nil
		}},
	{id: 11, message: "Can you help me fill in new products?",
		settled: func(f *chatFixture) error {
			if err := expectCountOf(f, "Item", 2); err != nil {
				return err
			}
			if n := f.openPreviews(); n != 1 {
				return fmt.Errorf("open previews = %d, want 1", n)
			}
			return expectEveryFieldSuggested(f)
		},
		answered: func(reply string) bool {
			return strings.Contains(reply, "Soda Fanta 1L") && strings.Contains(reply, "Save this?")
		}},
	{id: 12, message: "Can you help me fill in new products?",
		settled: func(f *chatFixture) error {
			if err := expectCountOf(f, "Item", 2); err != nil {
				return err
			}
			if n := f.openPreviews(); n != 0 {
				return fmt.Errorf("open previews = %d, want 0: the model guessed before it asked", n)
			}
			return nil
		},
		answered: func(reply string) bool { return strings.HasSuffix(strings.TrimSpace(reply), "?") }},
}

// expectEveryFieldSuggested checks the open preview: the person gave no values, so every field it saves is marked as a
// suggestion.
func expectEveryFieldSuggested(f *chatFixture) error {
	var p struct {
		Preview struct {
			Fields    map[string]any `json:"fields"`
			Suggested map[string]any `json:"suggested"`
		} `json:"preview"`
	}
	if err := json.Unmarshal([]byte(f.one("SELECT preview_json FROM _kora_ai_pending_write WHERE site = ? AND state = ?", f.siteName, "previewed")), &p.Preview); err != nil {
		return fmt.Errorf("preview is not readable: %w", err)
	}
	if len(p.Preview.Fields) == 0 {
		return fmt.Errorf("preview has no fields")
	}
	for field := range p.Preview.Fields {
		if p.Preview.Suggested[field] != true {
			return fmt.Errorf("field %q is not marked as a suggestion", field)
		}
	}
	return nil
}

// expectCountOf checks the row count of a doctype.
func expectCountOf(f *chatFixture, doc string, want int) error {
	if got := f.count(doc); got != strconv.Itoa(want) {
		return fmt.Errorf("%s rows = %s, want %d", doc, got, want)
	}
	return nil
}

// expectNumber checks the first value of a query as a number. The query takes args after the query text.
func expectNumber(f *chatFixture, query string, want float64, args ...any) error {
	raw := f.one(query, args...)
	got, err := strconv.ParseFloat(raw, 64)
	if err != nil || got != want {
		return fmt.Errorf("%s = %s, want %v", query, raw, want)
	}
	return nil
}

// expectRows checks the row count of a doctype and one number in the row that where matches.
func expectRows(f *chatFixture, doc string, count int, where string, arg any, column string, want float64) error {
	if err := expectCountOf(f, doc, count); err != nil {
		return err
	}
	q := "SELECT " + f.dialect.QuoteIdent(column) + " FROM " + f.tbl(doc) + " WHERE " + where
	return expectNumber(f, q, want, arg)
}

// recordTaskScripts are the scripted turns of each task for TestRecordTaskEvals and the fake provider of the live
// harness. Each turn's steps are the model's answers, in order.
var recordTaskScripts = map[int]func(f *chatFixture) []scriptedRecordTurn{
	1: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[0].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("find_records", map[string]any{"record_type": "Item", "search": "Sugar 1kg"}),
					callStep("describe_record_type", map[string]any{"record_type": "Item"}),
					callStep("create_record", map[string]any{"record_type": "Item", "data": map[string]any{"item_name": "Sugar 1kg", "price": 140}}),
					textStep("Preview ready."),
				}
			}, check: func(f *chatFixture, _ []string) error { return expectCountOf(f, "Item", 2) }},
			{message: "yes", steps: func(f *chatFixture) []packChatStep {
				return []packChatStep{confirmStep(f, "Sugar 1kg"), textStep("Saved Sugar 1kg at 140.")}
			}},
		}
	},
	2: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[1].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("describe_record_type", map[string]any{"record_type": "Customer"}),
					textStep("These packs have no customer type, so I cannot save Jane Wanjiru. Nothing was saved."),
				}
			}, check: func(_ *chatFixture, bodies []string) error {
				if len(bodies) < 2 || !strings.Contains(bodies[1], "not_found") {
					return fmt.Errorf("describe_record_type on Customer did not return not_found")
				}
				return nil
			}},
		}
	},
	3: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[2].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("find_records", map[string]any{"record_type": "Item", "search": "Sugar 2kg"}),
					callStep("update_record", map[string]any{"record_type": "Item", "name": "ITM-1", "data": map[string]any{"price": 260}}),
					textStep("Preview ready."),
				}
			}, check: func(f *chatFixture, _ []string) error {
				return expectNumber(f, "SELECT price FROM "+f.tbl("Item")+" WHERE name = ?", 250, "ITM-1")
			}},
			{message: "yes", steps: func(f *chatFixture) []packChatStep {
				return []packChatStep{confirmStep(f, "ITM-1"), textStep("Saved the new price, 260.")}
			}},
		}
	},
	4: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[3].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("list_actions", map[string]any{"record_type": "Till Session"}),
					callStep("run_action", map[string]any{"action": recordTaskPayOut, "input": map[string]any{"till_session": recordTaskTill, "amount": 500, "reason": "Transport"}}),
					textStep("Preview ready."),
				}
			}, check: func(f *chatFixture, _ []string) error { return expectCountOf(f, "Cash Movement", 0) }},
			{message: "yes", steps: func(f *chatFixture) []packChatStep {
				return []packChatStep{confirmStep(f, "Pay out"), textStep("Paid out 500 for transport.")}
			}},
		}
	},
	5: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[4].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("find_records", map[string]any{"record_type": "Sale", "limit": 5}),
					textStep("I cannot rank sales by item count with these tools, so I cannot name the sale with the most items."),
				}
			}, check: func(_ *chatFixture, bodies []string) error {
				if len(bodies) < 2 || !strings.Contains(bodies[1], "No Sale found.") {
					return fmt.Errorf("find_records on Sale did not report that no sale exists")
				}
				return nil
			}},
		}
	},
	6: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[5].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("find_records", map[string]any{"record_type": "Item"}),
					textStep("We have 2 products: Sugar 2kg and Salt 500g."),
				}
			}, check: func(_ *chatFixture, bodies []string) error {
				if len(bodies) < 2 || !strings.Contains(bodies[1], "Found 2 Item") {
					return fmt.Errorf("find_records on Item did not report 2")
				}
				return nil
			}},
		}
	},
	7: func(f *chatFixture) []scriptedRecordTurn {
		create := func(name string, price int) packChatStep {
			return callStep("create_record", map[string]any{"record_type": "Item", "data": map[string]any{"item_name": name, "price": price}})
		}
		return []scriptedRecordTurn{
			{message: recordTasks[6].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					create("Tea", 50), create("Milk", 60), create("Bread", 55),
					textStep("Preview ready."),
				}
			}, check: func(f *chatFixture, _ []string) error {
				// Three previews in consecutive window rounds, one turn: all three stay open until yes.
				if n := f.openPreviews(); n != 3 {
					return fmt.Errorf("open previews after the turn = %d, want 3", n)
				}
				return expectCountOf(f, "Item", 2)
			}},
			{message: "yes", steps: func(f *chatFixture) []packChatStep {
				return []packChatStep{
					confirmStep(f, "Tea"), confirmStep(f, "Milk"), confirmStep(f, "Bread"),
					textStep("Saved Tea, Milk and Bread."),
				}
			}},
		}
	},
	8: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[7].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("create_record", map[string]any{"record_type": "Item", "data": map[string]any{}}),
					textStep("What is the product name, and what is its price?"),
				}
			}, check: func(_ *chatFixture, bodies []string) error {
				if len(bodies) < 2 || !strings.Contains(bodies[1], "missing_fields") {
					return fmt.Errorf("create_record with no fields did not return missing_fields")
				}
				return nil
			}},
		}
	},
	9: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[8].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("find_records", map[string]any{"record_type": "Item", "search": "Salt 500g"}),
					textStep("I cannot delete records in this chat, so Salt 500g is still saved. Delete is not available here."),
				}
			}, check: func(_ *chatFixture, bodies []string) error {
				if len(bodies) == 0 || strings.Contains(bodies[0], `_delete"`) {
					return fmt.Errorf("a delete tool was offered to the model")
				}
				return nil
			}},
		}
	},
	11: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[10].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("find_records", map[string]any{"record_type": "Item", "search": "Soda Fanta"}),
					callStep("describe_record_type", map[string]any{"record_type": "Item"}),
					callStep("create_record", map[string]any{"record_type": "Item", "data": map[string]any{"item_name": "Soda Fanta 1L", "uom": "Nos", "price": 70, "cost": 50}}),
					textStep("Preview ready."),
				}
			}, check: func(_ *chatFixture, bodies []string) error {
				if len(bodies) > 4 {
					return fmt.Errorf("the turn took %d rounds, want at most 4", len(bodies))
				}
				return nil
			}},
		}
	},
	12: func(f *chatFixture) []scriptedRecordTurn {
		return []scriptedRecordTurn{
			{message: recordTasks[11].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("find_records", map[string]any{"record_type": "Item"}),
					textStep("What is the product name, and what is its price?"),
				}
			}, check: func(_ *chatFixture, bodies []string) error {
				if len(bodies) > 2 {
					return fmt.Errorf("the turn took %d rounds, want at most 2", len(bodies))
				}
				return nil
			}},
		}
	},
	10: func(f *chatFixture) []scriptedRecordTurn {
		sale := map[string]any{
			"register": "REG-1", "location": "LOC-1", "till_session": recordTaskTill, "currency": "KES",
			"items":    []any{map[string]any{"item": "ITM-1", "quantity": 2, "unit_price": 250, "amount": 500}},
			"payments": []any{map[string]any{"method": "Cash", "amount": 500, "currency": "KES"}},
		}
		return []scriptedRecordTurn{
			{message: recordTasks[9].message, steps: func(*chatFixture) []packChatStep {
				return []packChatStep{
					callStep("list_actions", map[string]any{"action": recordTaskRecordSale}),
					callStep("run_action", map[string]any{"action": recordTaskRecordSale, "input": sale}),
					textStep("Preview ready."),
				}
			}, check: func(f *chatFixture, _ []string) error { return expectCountOf(f, "Sale", 0) }},
			{message: "yes", steps: func(f *chatFixture) []packChatStep {
				return []packChatStep{confirmStep(f, "Record sale"), textStep("Sold 2 Sugar 2kg for 500 KES in cash.")}
			}},
		}
	},
}
