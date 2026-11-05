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
	if len(def.Steps) != 3 || def.Steps[0].As != "sale" || def.Steps[1].ForEach != "$input.items" || def.Steps[2].ForEach != "$input.items" {
		t.Fatalf("steps = %#v", def.Steps)
	}
	if got := def.Steps[1].Update.Adjust["quantity"]; got.Decrease != "$item.quantity" || got.Min != "0" {
		t.Fatalf("stock must decrease without going negative: %+v", got)
	}
	if def.Output["reference"] != "$steps.sale.name" || def.Output["total"] != "$computed.total" {
		t.Fatalf("receipt output = %v", def.Output)
	}
}
