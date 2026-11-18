//go:build integration

package kernel_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/kernel"
)

// saveSaleYAML creates a Sale under a reserved number of the till, or updates the
// sale already under that number. A new sale is refused past twenty for its register;
// the sale being updated is not counted against that limit.
const saveSaleYAML = `
name: save_sale
namespace: pos
version: 1
input:
  record: Sale
  extra:
    till_session: {type: string, required: true}
    sale_number: {type: string, required: true}
    register: {type: string, required: true}
require:
  - count:
      record: Sale
      where: {register: $input.register}
      limit: 20
      except: $input.sale_number
    message: Held sales limit reached
transaction:
  - upsert:
      record: Sale
      name: $input.sale_number
      in_block: $input.till_session
      values: {register: $input.register}
`

// TestUpsertStepCreatesThenUpdatesAcrossDialects saves twenty sales under one
// register, then checks that the twenty-first is refused, that an existing sale
// at exactly twenty can still be updated, and that a number outside the block
// cannot create a sale.
func TestUpsertStepCreatesThenUpdatesAcrossDialects(t *testing.T) {
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
			mustRegister(t, k, saveSaleYAML)
			payload, _ := json.Marshal(map[string]any{"data": map[string]any{"till_session": "till-1"}})
			mustComplete(t, exec(t, s, k, kernel.Operation{Command: "pos.reserve_numbers", Payload: payload, Context: opCtx(s.Name, "Administrator")}))
			var prefix string
			var first int64
			if err := s.DB.QueryRow(blockLookupSQL(tc.driver), "till-1").Scan(&prefix, &first); err != nil {
				t.Fatal(err)
			}
			number := func(n int64) string { return fmt.Sprintf("%s%05d", prefix, first+n) }
			save := func(n int64, register string) contract.CommandResult {
				p, _ := json.Marshal(map[string]any{"data": map[string]any{"till_session": "till-1", "sale_number": number(n), "register": register}})
				return exec(t, s, k, kernel.Operation{Command: "pos.save_sale", Payload: p, Context: opCtx(s.Name, "Administrator")})
			}
			for n := int64(0); n < 20; n++ {
				mustComplete(t, save(n, "R1"))
			}
			if res := save(20, "R1"); res.Status == contract.StatusCompleted {
				t.Fatal("twenty-first sale on R1 was created")
			}
			// Exactly twenty on R1: updating one of them is allowed, and moves it.
			mustComplete(t, save(5, "R1"))
			mustComplete(t, save(5, "R2"))
			var register string
			if err := s.DB.QueryRow(saleRegisterSQL(tc.driver), number(5)).Scan(&register); err != nil || register != "R2" {
				t.Fatalf("updated sale register = %q, err %v; want R2", register, err)
			}
			if res := save(0, "R1"); res.Status != contract.StatusCompleted {
				t.Fatalf("update at the limit refused: %+v", res.Error)
			}
			var total int
			if err := s.DB.QueryRow(countSalesSQL(tc.driver)).Scan(&total); err != nil || total != 20 {
				t.Fatalf("sales = %d, err %v; want 20 (updates never create)", total, err)
			}
		})
	}
}

func saleRegisterSQL(driver string) string {
	if driver == "postgres" {
		return `SELECT "register" FROM "tabSale" WHERE "name" = $1`
	}
	return "SELECT register FROM `tabSale` WHERE name = ?"
}

func countSalesSQL(driver string) string {
	if driver == "postgres" {
		return `SELECT COUNT(*) FROM "tabSale"`
	}
	return "SELECT COUNT(*) FROM `tabSale`"
}
