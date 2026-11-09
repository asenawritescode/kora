package kernel

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const forEachYAML = `name: library.issue_batch
version: 1
input:
  record: Loan
transaction:
  - as: batch
    create:
      record: Loan Batch
      values:
        member: $input.member
  - for_each: $input.books
    create:
      record: Loan
      values:
        batch: $steps.batch.name
        book: $item.book
        due: $input.due
`

func TestForEachExpandsInputListAndResolvesReferences(t *testing.T) {
	def, err := ParseCommandResource([]byte(forEachYAML))
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"member": "MEM-1", "due": "2026-11-30", "books": []any{
		map[string]any{"book": "B-1"}, map[string]any{"book": "B-2"},
	}}
	instances, err := expandCommandSteps(def.Steps, refEnv{input: input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 3 || instances[1].index != 1 || instances[2].item["book"] != "B-2" {
		t.Fatalf("instances = %#v", instances)
	}
	env := refEnv{input: input, item: instances[2].item, steps: map[string]string{"batch": "LB-0001"}}
	for ref, want := range map[string]any{"$steps.batch.name": "LB-0001", "$item.book": "B-2", "$input.due": "2026-11-30", "literal": "literal"} {
		got, err := resolveEnvRef(ref, env)
		if err != nil || got != want {
			t.Fatalf("%s = %v, %v; want %v", ref, got, err, want)
		}
	}
	if _, err := resolveEnvRef("$item.book", refEnv{input: input}); err == nil {
		t.Fatal("$item resolved outside for_each")
	}
}

func TestForEachRejectsInvalidDefinitionsAndInputs(t *testing.T) {
	invalid := map[string]string{
		"item outside for_each": strings.Replace(forEachYAML, "member: $input.member", "member: $item.member", 1),
		"unknown alias":         strings.Replace(forEachYAML, "$steps.batch.name", "$steps.order.name", 1),
		"for_each not input":    strings.Replace(forEachYAML, "for_each: $input.books", "for_each: $steps.batch.name", 1),
		"aliased for_each":      strings.Replace(forEachYAML, "  - for_each: $input.books\n", "  - for_each: $input.books\n    as: line\n", 1),
		"bad alias":             strings.Replace(forEachYAML, "as: batch", "as: Batch-1", 1),
	}
	for name, yaml := range invalid {
		if _, err := ParseCommandResource([]byte(yaml)); err == nil {
			t.Fatalf("%s: definition was accepted", name)
		}
	}
	def, _ := ParseCommandResource([]byte(forEachYAML))
	for name, input := range map[string]map[string]any{
		"not a list":     {"books": "B-1"},
		"not objects":    {"books": []any{"B-1"}},
		"missing list":   {},
		"too many items": {"books": manyBooks(MaxCommandStepInstances + 1)},
	} {
		if _, err := expandCommandSteps(def.Steps, refEnv{input: input}, nil); err == nil {
			t.Fatalf("%s: input was accepted", name)
		}
	}
}

func manyBooks(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"book": "B"}
	}
	return out
}

func TestNowTodayActorRefs(t *testing.T) {
	now := time.Date(2026, 11, 5, 23, 59, 1, 0, time.UTC)
	env := refEnv{input: map[string]any{"actor": "mallory"}, now: now, actor: "alice"}
	for ref, want := range map[string]any{"$now": now.Format(time.RFC3339Nano), "$today": "2026-11-05", "$actor": "alice"} {
		got, err := resolveEnvRef(ref, env)
		if err != nil || got != want {
			t.Fatalf("%s = %v, %v; want %v", ref, got, err, want)
		}
	}
	again, _ := resolveEnvRef("$now", env)
	first, _ := resolveEnvRef("$now", env)
	if again != first {
		t.Fatal("$now not stable")
	}
}

func TestUnknownRefRootRejectedAtParse(t *testing.T) {
	_, err := ParseCommandResource([]byte(strings.Replace(forEachYAML, "$input.due", "$series.loan", 1)))
	if err == nil || !strings.Contains(err.Error(), "unknown reference root") {
		t.Fatalf("err = %v", err)
	}
	ok := strings.Replace(forEachYAML, "$input.due", "$today", 1)
	if _, err := ParseCommandResource([]byte(ok)); err != nil {
		t.Fatal(err)
	}
}

func TestWalkRefAbsentVersusInvalid(t *testing.T) {
	root := map[string]any{"a": map[string]any{"b": 1}, "s": "x", "n": nil}
	tests := []struct {
		path   string
		absent bool
		err    bool
	}{
		{"a.b", false, false},
		{"n", false, false},
		{"missing", true, true},
		{"a.missing", true, true},
		{"missing.deeper", true, true},
		{"s.deeper", false, true},
	}
	for _, tt := range tests {
		_, err := walkRef("$input."+tt.path, root, tt.path)
		if (err != nil) != tt.err || errors.Is(err, errRefAbsent) != tt.absent {
			t.Errorf("%s: err=%v absent=%v", tt.path, err, errors.Is(err, errRefAbsent))
		}
	}
}

const guardRowsYAML = `
name: sale.void
namespace: pos
version: 1
input:
  record: Sale
require:
  - exists:
      record: Sale
      where:
        name: $input.sale
    as: original
transaction:
  - as: marker
    create:
      record: Note
      values:
        text: x
  - for_each: $steps.original.items
    create:
      record: Note
      values:
        text: $item.item
`

func TestForEachGuardRowsParseAndExpand(t *testing.T) {
	def, err := ParseCommandResource([]byte(guardRowsYAML))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string][]map[string]any{"original.items": {{"item": "A"}, {"item": nil}}}
	instances, err := expandCommandSteps(def.Steps, refEnv{input: map[string]any{"items": []any{map[string]any{"item": "ignored"}}}}, rows)
	if err != nil || len(instances) != 3 || instances[1].item["item"] != "A" || instances[2].item["item"] != nil {
		t.Fatalf("instances = %#v, %v", instances, err)
	}
	if instances, err = expandCommandSteps(def.Steps, refEnv{input: nil}, nil); err != nil || len(instances) != 1 {
		t.Fatalf("empty table: %#v, %v", instances, err)
	}
	if _, err = expandCommandSteps(def.Steps, refEnv{input: nil}, map[string][]map[string]any{"original.items": make([]map[string]any, MaxCommandStepInstances+1)}); err == nil {
		t.Fatal("oversized table was accepted")
	}
	for name, yaml := range map[string]string{
		"transaction alias": strings.Replace(guardRowsYAML, "$steps.original.items", "$steps.marker.items", 1),
		"name field":        strings.Replace(guardRowsYAML, "$steps.original.items", "$steps.original.name", 1),
		"unknown alias":     strings.Replace(guardRowsYAML, "$steps.original.items", "$steps.ghost.items", 1),
	} {
		if _, err := ParseCommandResource([]byte(yaml)); err == nil {
			t.Fatalf("%s: definition was accepted", name)
		}
	}
}

func whereDef(t *testing.T, where, forEach string) (*CommandResource, error) {
	t.Helper()
	return ParseCommandResource([]byte("name: t.w\nversion: 1\ninput:\n  record: Loan\ntransaction:\n  - " + forEach + "\n    where: " + where + "\n    create:\n      record: Loan\n      values:\n        book: $input.m\n"))
}

func TestForEachWhereFiltersElements(t *testing.T) {
	pay := func(m string, n int) any { return map[string]any{"method": m, "n": n, "kind": "x"} }
	input := map[string]any{"m": "Cash", "p": []any{pay("Cash", 1), pay("Card", 2), pay("Cash", 3)}}
	for name, tc := range map[string]struct {
		where string
		want  int
	}{
		"match":     {"{method: Cash}", 2},
		"no match":  {"{method: Mpesa}", 0},
		"two keys":  {"{method: Cash, n: 3}", 1},
		"ref value": {"{method: $input.m}", 2},
		"missing":   {"{nokey: a}", 0},
	} {
		def, err := whereDef(t, tc.where, "for_each: $input.p")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := expandCommandSteps(def.Steps, refEnv{input: input}, nil)
		if err != nil || len(got) != tc.want {
			t.Fatalf("%s: %d instances, %v; want %d", name, len(got), err, tc.want)
		}
	}
}

func TestForEachWhereRejectedAtLoad(t *testing.T) {
	for name, tc := range map[string]struct{ where, forEach string }{
		"without for_each": {"{method: Cash}", "as: a"},
		"item value":       {"{method: $item.method}", "for_each: $input.p"},
		"unknown root":     {"{method: $bogus.x}", "for_each: $input.p"},
		"bad steps ref":    {"{method: $steps.nope.name}", "for_each: $input.p"},
	} {
		if _, err := whereDef(t, tc.where, tc.forEach); err == nil {
			t.Fatalf("%s: expected load error", name)
		}
	}
}

const stepFieldYAML = `name: till.open
namespace: pos
version: 1
input:
  record: Session
require:
  - exists:
      record: Register
      where:
        name: $input.register
    as: register
transaction:
  - as: session
    create:
      record: Session
      values:
        register: $steps.register.name
        location: $steps.register.location
  - create:
      record: Audit
      values:
        session: $steps.session.name
        where: $steps.session.location
output:
  location: $steps.register.location
`

func TestStepFieldReferencesParseAndResolve(t *testing.T) {
	if _, err := ParseCommandResource([]byte(stepFieldYAML)); err != nil {
		t.Fatalf("valid field references rejected: %v", err)
	}
	invalid := map[string]string{
		"nested path":      strings.Replace(stepFieldYAML, "$steps.register.location\n  - create", "$steps.register.location.x\n  - create", 1),
		"empty field":      strings.Replace(stepFieldYAML, "$steps.register.location\n  - create", "$steps.register.\n  - create", 1),
		"bad field":        strings.Replace(stepFieldYAML, "$steps.register.location\n  - create", "$steps.register.lo-cation\n  - create", 1),
		"unknown alias":    strings.Replace(stepFieldYAML, "$steps.register.location\n  - create", "$steps.ghost.location\n  - create", 1),
		"later alias":      strings.Replace(stepFieldYAML, "register: $steps.register.name", "register: $steps.session.location", 1),
		"output bad alias": strings.Replace(stepFieldYAML, "location: $steps.register.location\n", "location: $steps.ghost.location\n", 1),
	}
	for name, yaml := range invalid {
		if _, err := ParseCommandResource([]byte(yaml)); err == nil {
			t.Fatalf("%s: definition was accepted", name)
		}
	}

	type read struct{ alias, field string }
	var reads []read
	env := refEnv{
		steps: map[string]string{"register": "REG-1"},
		fieldReader: func(alias, field string) (any, error) {
			reads = append(reads, read{alias, field})
			return "Nairobi", nil
		},
	}
	tests := []struct {
		name    string
		ref     string
		env     refEnv
		want    any
		wantErr bool
		reads   int
	}{
		{"name needs no read", "$steps.register.name", env, "REG-1", false, 0},
		{"field reads through the reader", "$steps.register.location", env, "Nairobi", false, 1},
		{"unknown alias", "$steps.ghost.location", env, nil, true, 0},
		{"no reader means no field access", "$steps.register.location", refEnv{steps: env.steps}, nil, true, 0},
		{"nested path", "$steps.register.location.x", env, nil, true, 0},
		{"empty field", "$steps.register.", env, nil, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reads = nil
			got, err := resolveEnvRef(tc.ref, tc.env)
			if (err != nil) != tc.wantErr || got != tc.want || len(reads) != tc.reads {
				t.Fatalf("got %v, %v, reads %v; want %v, err=%v, reads=%d", got, err, reads, tc.want, tc.wantErr, tc.reads)
			}
		})
	}
}
