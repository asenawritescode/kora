package kernel

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestNumberInOpenBlockAcceptsOnlyReservedNumbersOfTheSeries(t *testing.T) {
	series, err := doctype.ParseNamingSeries("SALE-.YYYY.-.#####")
	if err != nil {
		t.Fatal(err)
	}
	blocks := []db.NumberBlockRow{
		{Doctype: "Sale", Prefix: "SALE-2026-", Owner: "till-1", State: "open", First: 1, Last: 50},
		{Doctype: "Sale", Prefix: "SALE-2026-", Owner: "till-2", State: "open", First: 51, Last: 100},
	}
	cases := []struct {
		name string
		want bool
	}{
		{"SALE-2026-00001", true},
		{"SALE-2026-00050", true},
		{"SALE-2026-00051", true},
		{"SALE-2026-00100", true},
		{"SALE-2026-00000", false},
		{"SALE-2026-00101", false},
		{"SALE-2026-0001", false},   // wrong width: not the series' own name
		{"SALE-2025-00001", false},  // another year's prefix
		{"SALE-2026-0000A", false},  // not a number
		{"SALE-2026-", false},       // no number
		{"XSALE-2026-00001", false}, // prefix must lead
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := numberInOpenBlock(series, blocks, tc.name); got != tc.want {
				t.Fatalf("numberInOpenBlock(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
	if numberInOpenBlock(series, nil, "SALE-2026-00001") {
		t.Fatal("no blocks must accept no number")
	}
}

func TestCreateStepPresetNameNeedsItsBlockOwnerAndTogether(t *testing.T) {
	const header = `name: hold_sale
namespace: pos
version: 1
module_owner_id: pos.core
capability: pos.checkout
operation: hold_sale
presentation:
  title: Hold sale
permission: create
input:
  record: Sale
  extra:
    till_session: {type: string, required: true}
    sale_number: {type: string, required: true}
transaction:
  - create:
      record: Sale
`
	cases := []struct {
		name, step string
		wantErr    bool
	}{
		{"plain create", "      values: {register: $input.till_session}\n", false},
		{"name with block", "      name: $input.sale_number\n      in_block: $input.till_session\n      values: {register: $input.till_session}\n", false},
		{"name without block", "      name: $input.sale_number\n      values: {register: $input.till_session}\n", true},
		{"block without name", "      in_block: $input.till_session\n      values: {register: $input.till_session}\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCommandResource([]byte(header + tc.step))
			if (err != nil) != tc.wantErr {
				t.Fatalf("parse error = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "in_block together") {
				t.Fatalf("error %q does not name the pairing rule", err)
			}
		})
	}
}

func TestCountGuardParsesAndRejectsIncompleteDefinitions(t *testing.T) {
	const header = `name: hold_sale
namespace: pos
version: 1
module_owner_id: pos.core
capability: pos.checkout
operation: hold_sale
presentation:
  title: Hold sale
permission: create
input:
  record: Sale
  extra:
    register: {type: string, required: true}
`
	const tail = `transaction:
  - create:
      record: Sale
      values: {register: $input.register}
`
	cases := []struct {
		name, require string
		wantErr       string
	}{
		{"valid", "require:\n  - count: {record: Sale, where: {register: $input.register, status: Held}, limit: 20}\n", ""},
		{"no limit", "require:\n  - count: {record: Sale, where: {register: $input.register}}\n", "a limit of at least 1"},
		{"zero limit", "require:\n  - count: {record: Sale, where: {register: $input.register}, limit: 0}\n", "a limit of at least 1"},
		{"no where", "require:\n  - count: {record: Sale, limit: 3}\n", "a limit of at least 1"},
		{"unless role", "require:\n  - count: {record: Sale, where: {register: $input.register}, limit: 3}\n    unless_role: [Owner]\n", "only valid on exists"},
		{"alias", "require:\n  - count: {record: Sale, where: {register: $input.register}, limit: 3}\n    as: held\n", "alias is only valid on exists"},
		{"two kinds", "require:\n  - count: {record: Sale, where: {register: $input.register}, limit: 3}\n    exists: {record: Sale, where: {register: $input.register}}\n", "exactly one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCommandResource([]byte(header + tc.require + tail))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("valid count guard rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
