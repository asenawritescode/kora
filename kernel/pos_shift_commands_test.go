package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPOSShiftCommandsParse(t *testing.T) {
	for name, wantGuard := range map[string]bool{"open_shift": true, "close_shift": true} {
		raw, err := os.ReadFile(filepath.Join("..", "config", "pos", "commands", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		def, err := ParseCommandResource(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (len(def.Require) > 0) != wantGuard {
			t.Fatalf("%s: guards = %#v", name, def.Require)
		}
	}
	raw, _ := os.ReadFile(filepath.Join("..", "config", "pos", "commands", "open_shift.yaml"))
	def, _ := ParseCommandResource(raw)
	if g := def.Require[1].NotExists; g == nil || g.Where["status"] != "Open" || g.Where["register"] != "$input.register" {
		t.Fatalf("second open is not refused per register: %#v", def.Require)
	}
}

func TestPOSTillCommandsParse(t *testing.T) {
	for _, name := range []string{"open_till", "close_till"} {
		raw, err := os.ReadFile(filepath.Join("..", "config", "pos", "commands", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		def, err := ParseCommandResource(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(def.Require) == 0 || len(def.Steps) != 1 {
			t.Fatalf("%s: require=%d steps=%d", name, len(def.Require), len(def.Steps))
		}
	}
}

func TestRecordSaleCommand(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "config", "pos", "commands", "record_sale.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def, err := ParseCommandResource(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Require) != 4 || len(def.Steps) != 5 || def.Steps[0].As != "sale" || def.Steps[1].ForEach != "$input.payments" || def.Steps[2].Create.Record != "Sale Line Balance" || def.Steps[3].ForEach != "$input.items" || def.Steps[4].ForEach != "$input.items" {
		t.Fatalf("steps = %#v", def.Steps)
	}
	if got := def.Steps[3].Update.Adjust["quantity"]; got.Decrease != "$item.quantity" || got.Min != "0" {
		t.Fatalf("stock must decrease without going negative: %+v", got)
	}
	if def.Steps[0].Create.Values["location"] != "$input.location" {
		t.Fatalf("sale must record its location: %v", def.Steps[0].Create.Values)
	}
	if def.Output["reference"] != "$steps.sale.name" || def.Output["total"] != "$computed.total" {
		t.Fatalf("receipt output = %v", def.Output)
	}
}

func TestPOSReturnAndVoidCommandsParse(t *testing.T) {
	for name, guards := range map[string]int{"return_sale": 6, "void_sale": 7} {
		raw, err := os.ReadFile(filepath.Join("..", "config", "pos", "commands", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		def, err := ParseCommandResource(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(def.Require) != guards {
			t.Fatalf("%s: guards = %d, want %d", name, len(def.Require), guards)
		}
		if got := def.Steps[len(def.Steps)-3].Update.Adjust["quantity"]; got.Increase != "$item.quantity" {
			t.Fatalf("%s must restock: %+v", name, got)
		}
	}
}

// TestPOSOpenSessionCommandParse (#326): one command opens the shift and the
// till together and stamps register, location and cashier on the till.
func TestPOSOpenSessionCommandParse(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "config", "pos", "commands", "open_session.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def, err := ParseCommandResource(raw)
	if err != nil {
		t.Fatal(err)
	}
	if def.Input.Record != "Till Session" || len(def.Steps) != 2 || def.Steps[0].As != "shift" {
		t.Fatalf("open_session steps = %#v", def.Steps)
	}
	if v := def.Steps[0].Create.Values; v["register"] != "$input.register" || v["cashier"] != "$actor" || v["status"] != "Open" {
		t.Fatalf("shift values = %v", v)
	}
	if v := def.Steps[1].Create.Values; def.Steps[1].Create.Record != "Till Session" || v["shift"] != "$steps.shift.name" || v["register"] != "$input.register" || v["location"] != "$input.location" || v["cashier"] != "$actor" || v["expected_cash"] != "$input.opening_float" {
		t.Fatalf("till values = %v", v)
	}
	var registerOpen, cashierOpen, locationChecked bool
	for _, g := range def.Require {
		switch {
		case g.NotExists != nil && g.NotExists.Record == "Shift":
			registerOpen = g.NotExists.Where["register"] == "$input.register" && g.NotExists.Where["status"] == "Open"
		case g.NotExists != nil && g.NotExists.Record == "Till Session":
			cashierOpen = g.NotExists.Where["cashier"] == "$actor" && g.NotExists.Where["status"] == "Open"
		case g.Exists != nil && g.Exists.Record == "Register":
			locationChecked = g.Exists.Where["location"] == "$input.location"
		}
	}
	if !registerOpen || !cashierOpen || !locationChecked {
		t.Fatalf("guards register=%v cashier=%v location=%v", registerOpen, cashierOpen, locationChecked)
	}
}

// TestPOSTillCommandsRequireOwnCashier (#358): every command that takes a
// till or shift refuses a till/shift whose cashier is not the actor, except
// for Owner, Manager and Administrator.
func TestPOSTillCommandsRequireOwnCashier(t *testing.T) {
	for name, record := range map[string]string{
		"record_sale": "Till Session", "return_sale": "Till Session", "void_sale": "Till Session",
		"close_till": "Till Session", "close_shift": "Shift",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "config", "pos", "commands", name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			def, err := ParseCommandResource(raw)
			if err != nil {
				t.Fatal(err)
			}
			found := 0
			for _, g := range def.Require {
				if g.Exists == nil || g.Exists.Where["cashier"] != "$actor" {
					continue
				}
				found++
				if g.Exists.Record != record || len(g.UnlessRole) != 3 || g.UnlessRole[0] != "Owner" || g.UnlessRole[1] != "Manager" || g.UnlessRole[2] != "Administrator" || g.Message == "" {
					t.Fatalf("cashier guard = %+v", g)
				}
			}
			if found != 1 {
				t.Fatalf("cashier guards = %d, want 1", found)
			}
		})
	}
}

// TestPOSReturnSaleRefundMustEqualReturnTotal (#336): a return records a refund
// payment line for exactly the returned total, so the till and refunds_by_method
// see it, and the line needs no payment-intent reference.
func TestPOSReturnSaleRefundMustEqualReturnTotal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "config", "pos", "commands", "return_sale.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	def, err := ParseCommandResource(raw)
	if err != nil {
		t.Fatal(err)
	}
	var equal []string
	for _, g := range def.Require {
		if g.Payment != nil {
			t.Fatal("a refund must not consume a payment intent")
		}
		if len(g.Equal) > 0 {
			equal = g.Equal
		}
	}
	if len(equal) != 2 || equal[0] != "$computed.paid" || equal[1] != "$computed.total" {
		t.Fatalf("equal guard = %v, want [$computed.paid $computed.total]", equal)
	}
}
