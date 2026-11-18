package kernel

import (
	"fmt"
	"strings"
	"testing"
)

// whenDef builds a command with one Stock Entry step guarded by when. extra is
// the input.extra block, and forEach or as add the matching step shape.
func whenDef(when, extra string, forEach, as bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name: pos.test_when\nversion: 1\ninput:\n  record: Sale\n  extra:\n%s", extra)
	b.WriteString("compute:\n  n: {count: $input.items}\ntransaction:\n")
	if as {
		b.WriteString("  - as: st\n")
	} else {
		b.WriteString("  - \n")
	}
	if forEach {
		b.WriteString("    for_each: $input.items\n")
	}
	quantity := "1"
	if forEach {
		quantity = "$item.quantity"
	}
	fmt.Fprintf(&b, "    when: %q\n    create:\n      record: Stock Entry\n      values:\n        quantity: %s\n", when, quantity)
	return b.String()
}

const restockExtra = "    restock: {type: boolean, default: false}\n"

func TestWhenGatesStepsAndForEachAtLoadAndRun(t *testing.T) {
	items := []any{map[string]any{"quantity": "1"}, map[string]any{"quantity": "2"}}
	tests := []struct {
		name       string
		when       string
		extra      string
		forEach    bool
		as         bool
		input      map[string]any
		want       int
		wantLoad   bool // true when the definition must be refused at load
		wantRunErr bool // true when the condition errors at run time
	}{
		{name: "true runs", when: "$input.restock = true", extra: restockExtra, input: map[string]any{"restock": true, "items": items}, want: 1},
		{name: "false skips", when: "$input.restock = true", extra: restockExtra, input: map[string]any{"restock": false, "items": items}, want: 0},
		{name: "bare boolean ref", when: "$input.restock", extra: restockExtra, input: map[string]any{"restock": true, "items": items}, want: 1},
		{name: "for_each true runs each element", when: "$input.restock = true", extra: restockExtra, forEach: true, input: map[string]any{"restock": true, "items": items}, want: 2},
		{name: "for_each false skips whole loop", when: "$input.restock = true", extra: restockExtra, forEach: true, input: map[string]any{"restock": false, "items": items}, want: 0},
		{name: "computed ref true", when: `$computed.n = "2"`, extra: restockExtra, input: map[string]any{"restock": false, "items": items}, want: 1},
		{name: "computed ref false", when: `$computed.n = "3"`, extra: restockExtra, input: map[string]any{"restock": false, "items": items}, want: 0},
		{name: "non-boolean condition errors at run", when: "$input.restock", extra: "    restock: {type: string}\n", input: map[string]any{"restock": "yes", "items": items}, wantRunErr: true},
		{name: "unknown input refused at load", when: "$input.missing = true", extra: restockExtra, wantLoad: true},
		{name: "unknown compute key refused at load", when: `$computed.nope = "2"`, extra: restockExtra, wantLoad: true},
		{name: "steps ref refused at load", when: "$steps.x.name = 1", extra: restockExtra, wantLoad: true},
		{name: "function refused at load", when: "SUM(input.items) = 1", extra: restockExtra, wantLoad: true},
		{name: "as with when refused at load", when: "$input.restock", extra: restockExtra, as: true, wantLoad: true},
		{name: "default on string refused at load", when: "$input.restock", extra: "    restock: {type: string, default: false}\n", wantLoad: true},
		{name: "default with required refused at load", when: "$input.restock", extra: "    restock: {type: boolean, default: false, required: true}\n", wantLoad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def, err := ParseCommandResource([]byte(whenDef(tc.when, tc.extra, tc.forEach, tc.as)))
			if tc.wantLoad {
				if err == nil {
					t.Fatal("want load error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			computed, err := evalCompute(def.Compute, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			instances, err := expandCommandSteps(def.Steps, refEnv{input: tc.input, computed: computed}, nil)
			if tc.wantRunErr {
				if err == nil {
					t.Fatal("want run error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(instances) != tc.want {
				t.Fatalf("instances = %d, want %d", len(instances), tc.want)
			}
		})
	}
}

func TestInputDefaultsFillOnlyOmittedExtras(t *testing.T) {
	def, err := ParseCommandResource([]byte(whenDef("$input.restock", restockExtra, false, false)))
	if err != nil {
		t.Fatal(err)
	}
	omitted := map[string]any{}
	def.applyInputDefaults(omitted)
	if omitted["restock"] != false {
		t.Fatalf("omitted restock = %v, want false", omitted["restock"])
	}
	given := map[string]any{"restock": true}
	def.applyInputDefaults(given)
	if given["restock"] != true {
		t.Fatalf("given restock = %v, want true", given["restock"])
	}
}
