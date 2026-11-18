package kernel

import (
	"strings"
	"testing"
)

func TestReserveStepParsesAndRejectsIncompleteDefinitions(t *testing.T) {
	const header = `name: reserve_numbers
namespace: pos
version: 1
module_owner_id: pos.core
capability: pos.checkout
operation: reserve_numbers
permission: create
input:
  record: Till Session
  extra:
    till_session: {type: string, required: true}
`
	cases := []struct {
		name    string
		step    string
		wantErr string
	}{
		{"complete", "transaction:\n  - as: block\n    reserve: {record: Sale, owner: $input.till_session, size: 50}\noutput:\n  reference: $steps.block.name\n", ""},
		{"zero size", "transaction:\n  - reserve: {record: Sale, owner: $input.till_session, size: 0}\n", "reserve requires record, owner and a positive size"},
		{"no owner", "transaction:\n  - reserve: {record: Sale, size: 50}\n", "reserve requires record, owner and a positive size"},
		{"two kinds", "transaction:\n  - reserve: {record: Sale, owner: $input.till_session, size: 5}\n    create: {record: Sale, values: {status: Draft}}\n", "must set exactly one of create, update or ensure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCommandResource([]byte(header + tc.step))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
