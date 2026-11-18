//go:build integration
// +build integration

package kernel_test

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
)

const moveCashYAML = `
name: move_cash
namespace: till
version: 1
input:
  record: Till Session
  extra:
    till_session: {type: string, required: true}
    direction: {type: string, required: true}
    amount: {type: string, required: true}
transaction:
  - update:
      record: Till Session
      name: $input.till_session
      adjust:
        expected_cash: {by: $input.amount, sign_from: $input.direction, increase_when: In, decrease_when: Out, min: "0"}
`

// TestSignedAdjustMovesBalanceByDirectionOnBothDialects runs one command whose
// adjust takes its sign from an input value. It checks that In adds, Out
// subtracts, a floor refuses an overdraw, and an unknown direction is refused,
// and that refused commands leave the balance unchanged.
func TestSignedAdjustMovesBalanceByDirectionOnBothDialects(t *testing.T) {
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_KERNEL_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_KERNEL_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.driver, func(t *testing.T) {
			if os.Getenv(tc.env) == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			session := &doctype.DocType{
				Name: "Till Session", Module: "Kernel Test",
				Fields: []doctype.Field{
					{Fieldname: "status", Fieldtype: "Data", Label: "Status"},
					{Fieldname: "expected_cash", Fieldtype: "Currency", Label: "Expected Cash"},
				},
			}
			perms := []*doctype.Permission{{Doctype: "Till Session", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true}}
			s := newDisposableFixture(t, tc.driver, tc.env, []*doctype.DocType{session}, perms)
			defer s.DB.Close()
			k := newKernel(s)
			k.Commands = kernel.NewCommandRegistry()
			mustRegister(t, k, moveCashYAML)

			seed, _ := json.Marshal(map[string]any{"doctype": "Till Session", "data": map[string]any{"status": "Open", "expected_cash": "100"}})
			mustComplete(t, exec(t, s, k, kernel.Operation{Command: "record.create", Payload: seed, Context: opCtx(s.Name, "Administrator")}))
			table := db.Rebind(s.Dialect, "SELECT name FROM "+s.Dialect.QuoteIdent(session.RawTableName()))
			var till string
			if err := s.DB.QueryRow(table).Scan(&till); err != nil {
				t.Fatalf("read till: %v", err)
			}

			move := func(direction string, amount float64) contract.CommandResult {
				payload, _ := json.Marshal(map[string]any{"data": map[string]any{"till_session": till, "direction": direction, "amount": amount}})
				return exec(t, s, k, kernel.Operation{Command: "till.move_cash", Payload: payload, Context: opCtx(s.Name, "Administrator")})
			}
			balance := func() string {
				var v string
				query := db.Rebind(s.Dialect, "SELECT expected_cash FROM "+s.Dialect.QuoteIdent(session.RawTableName())+" WHERE name = ?")
				if err := s.DB.QueryRow(query, till).Scan(&v); err != nil {
					t.Fatalf("read balance: %v", err)
				}
				return v
			}
			expect := func(want string) {
				t.Helper()
				got, ok := new(big.Rat).SetString(balance())
				wantRat, _ := new(big.Rat).SetString(want)
				if !ok || got.Cmp(wantRat) != 0 {
					t.Fatalf("expected_cash = %s, want %s", balance(), want)
				}
			}

			mustComplete(t, move("In", 12.5))
			expect("112.5")
			mustComplete(t, move("Out", 30))
			expect("82.5")

			refused := move("Out", 100)
			if refused.Status == contract.StatusCompleted || refused.Error == nil || refused.Error.Type != contract.CodeConflict {
				t.Fatalf("overdraw not refused as CONFLICT: %+v", refused)
			}
			expect("82.5")

			unknown := move("Sideways", 1)
			if unknown.Status == contract.StatusCompleted || unknown.Error == nil || unknown.Error.Type != contract.CodeValidationFailed {
				t.Fatalf("unknown direction not refused: %+v", unknown)
			}
			expect("82.5")
			if unknown.Error.Message == "" {
				t.Fatal("refusal carries no message")
			}
		})
	}
}
