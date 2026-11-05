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
