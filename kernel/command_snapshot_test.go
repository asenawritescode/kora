package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

const lendBookYAML = `name: library.lend_book
version: 1
input:
  record: Loan
transaction:
  - create:
      record: Loan
      values:
        book: $input.book
        member: $input.member
`

func TestCommandResourceJSONRoundTrip(t *testing.T) {
	def, err := ParseCommandResource([]byte(lendBookYAML))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeCommandResources([]*CommandResource{def})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCommandResources(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].FullName() != "library.lend_book" || decoded[0].Steps[0].Create.Values["member"] != "$input.member" {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestDecodeCommandResourcesRejectsInvalidDefinitions(t *testing.T) {
	cases := []struct {
		name string
		raw  []json.RawMessage
		want string
	}{
		{"unknown key", []json.RawMessage{json.RawMessage(`{"name":"library.lend_book","input":{"record":"Loan"},"transaction":[{"create":{"record":"Loan","values":{"a":"b"}}}],"surprise":true}`)}, "unknown field"},
		{"empty transaction", []json.RawMessage{json.RawMessage(`{"name":"library.lend_book","input":{"record":"Loan"},"transaction":[]}`)}, "empty transaction"},
		{"duplicate", []json.RawMessage{
			json.RawMessage(`{"name":"library.lend_book","input":{"record":"Loan"},"transaction":[{"create":{"record":"Loan","values":{"a":"b"}}}]}`),
			json.RawMessage(`{"name":"library.lend_book","input":{"record":"Loan"},"transaction":[{"create":{"record":"Loan","values":{"a":"b"}}}]}`),
		}, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCommandResources(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCommandRegistryExtendLayersWithoutMutatingBase(t *testing.T) {
	base := NewCommandRegistry()
	global, _ := ParseCommandResource([]byte(strings.Replace(lendBookYAML, "library.lend_book", "library.global_note", 1)))
	if err := base.Register(global); err != nil {
		t.Fatal(err)
	}
	tenantDef, _ := ParseCommandResource([]byte(lendBookYAML))
	extended, err := base.Extend([]*CommandResource{tenantDef})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := extended.Lookup("library.lend_book"); !ok {
		t.Fatal("tenant command missing from extended registry")
	}
	if _, ok := extended.Lookup("library.global_note"); !ok {
		t.Fatal("base command missing from extended registry")
	}
	if _, ok := base.Lookup("library.lend_book"); ok {
		t.Fatal("base registry was mutated")
	}
	if _, err := base.Extend([]*CommandResource{global}); err == nil {
		t.Fatal("expected collision with base command to be rejected")
	}
}
