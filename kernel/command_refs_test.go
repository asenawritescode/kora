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
	instances, err := expandCommandSteps(def.Steps, input)
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
		if _, err := expandCommandSteps(def.Steps, input); err == nil {
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
