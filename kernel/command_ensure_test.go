package kernel

import (
	"strings"
	"testing"
)

const ensureYAML = `name: shelf.receive
namespace: library
version: 1
input:
  record: Shelf
transaction:
  - as: shelf
    ensure:
      record: Shelf
      where:
        book: $input.book
        room: $input.room
      values:
        copies: "0"
  - update:
      record: Shelf
      name: $steps.shelf.name
      adjust:
        copies: { increase: $input.copies }
`

func TestEnsureStepParsesAndRejectsInvalidDefinitions(t *testing.T) {
	def, err := ParseCommandResource([]byte(ensureYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.TouchedRecords(); len(got) != 1 || got[0] != "Shelf" {
		t.Fatalf("touched records = %v", got)
	}
	tests := []struct{ name, from, to string }{
		{"no where", "      where:\n        book: $input.book\n        room: $input.room\n", ""},
		{"where and values overlap", `copies: "0"`, `book: x`},
		{"values reference an item outside for_each", `copies: "0"`, `copies: $item.n`},
		{"ensure and create together", "  - update:", "    create:\n      record: Shelf\n      values:\n        book: x\n  - update:"},
		{"unknown reference root", "book: $input.book", "book: $bogus.book"},
		{"no record", "    ensure:\n      record: Shelf\n", "    ensure:\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseCommandResource([]byte(strings.Replace(ensureYAML, tc.from, tc.to, 1))); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
