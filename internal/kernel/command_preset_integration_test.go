//go:build integration

package kernel_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/kernel"
)

// holdSaleYAML creates a Sale under a preset number, which must sit in an open
// block of the till named by in_block, and refuses a fourth sale on a register.
const holdSaleYAML = `
name: hold_sale
namespace: pos
version: 1
input:
  record: Sale
  extra:
    till_session: {type: string, required: true}
    sale_number: {type: string, required: true}
    register: {type: string, required: true}
require:
  - count: {record: Sale, where: {register: $input.register}, limit: 3}
    message: Held sales limit reached
transaction:
  - create:
      record: Sale
      name: $input.sale_number
      in_block: $input.till_session
      values: {register: $input.register}
`

// TestPresetSaleNumberAndCountLimitAcrossDialects reserves a block for each of two
// tills, then checks that hold_sale keeps a reserved number, refuses a number
// outside the till's block, one already used, and the sale past the count limit.
func TestPresetSaleNumberAndCountLimitAcrossDialects(t *testing.T) {
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			s := newReserveFixture(t, tc.driver, tc.env)
			defer s.DB.Close()
			k := newKernel(s)
			k.Commands = kernel.NewCommandRegistry()
			mustRegister(t, k, reserveNumbersYAML)
			mustRegister(t, k, holdSaleYAML)

			for _, session := range []string{"till-1", "till-2"} {
				payload, _ := json.Marshal(map[string]any{"data": map[string]any{"till_session": session}})
				mustComplete(t, exec(t, s, k, kernel.Operation{Command: "pos.reserve_numbers", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
			}
			// number returns the n-th reserved number of a till's block, as a name.
			number := func(owner string, n int64) string {
				t.Helper()
				var prefix string
				var first int64
				if err := s.DB.QueryRow(blockLookupSQL(tc.driver), owner).Scan(&prefix, &first); err != nil {
					t.Fatalf("read block of %s: %v", owner, err)
				}
				return fmt.Sprintf("%s%05d", prefix, first+n)
			}
			hold := func(session, saleNumber, register string) contract.CommandResult {
				payload, _ := json.Marshal(map[string]any{"data": map[string]any{"till_session": session, "sale_number": saleNumber, "register": register}})
				return exec(t, s, k, kernel.Operation{Command: "pos.hold_sale", Payload: payload, Context: opCtx(s.Name, "Administrator")})
			}
			refused := func(label string, res contract.CommandResult, code string) {
				t.Helper()
				if res.Status == contract.StatusCompleted || res.Error == nil || string(res.Error.Type) != code {
					t.Fatalf("%s: status=%s error=%+v, want refusal %s", label, res.Status, res.Error, code)
				}
			}

			mustComplete(t, hold("till-1", number("till-1", 0), "R1"))
			refused("reused number", hold("till-1", number("till-1", 0), "R1"), "VALIDATION_FAILED")
			refused("number outside the block", hold("till-1", "SALE-2026-99999", "R1"), "VALIDATION_FAILED")
			refused("another till's number", hold("till-1", number("till-2", 0), "R1"), "VALIDATION_FAILED")
			mustComplete(t, hold("till-1", number("till-1", 1), "R1"))
			mustComplete(t, hold("till-2", number("till-2", 1), "R1"))
			refused("fourth sale on R1", hold("till-1", number("till-1", 2), "R1"), "CONFLICT")
			mustComplete(t, hold("till-1", number("till-1", 2), "R2"))
		})
	}
}

// blockLookupSQL returns the open block row of one till, for the dialect.
func blockLookupSQL(driver string) string {
	if driver == "postgres" {
		return `SELECT "prefix", "first_number" FROM "_kora_number_block" WHERE "owner" = $1`
	}
	return "SELECT prefix, first_number FROM _kora_number_block WHERE owner = ?"
}
