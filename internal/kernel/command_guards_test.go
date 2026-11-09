package kernel

import (
	"github.com/asenawritescode/kora/contract"
	"strings"
	"testing"
)

const guardYAML = `name: library.renew_loan
version: 1
input:
  record: Loan
require:
  - lock:
      record: Member
      name: $input.member
  - exists:
      record: Loan
      where:
        member: $input.member
        status: Active
    as: loan
    message: Member has no active loan
  - not_exists:
      record: Fine
      where:
        member: $input.member
        status: Unpaid
    message: Member has unpaid fines
transaction:
  - update:
      record: Loan
      name: $steps.loan.name
      values:
        due: $input.due
`

func TestCommandGuardsParseAndBindAliases(t *testing.T) {
	def, err := ParseCommandResource([]byte(guardYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Require) != 3 || def.Require[1].As != "loan" || def.Require[2].NotExists.Where["status"] != "Unpaid" {
		t.Fatalf("require = %#v", def.Require)
	}
}

func TestCommandGuardsRejectInvalidDefinitions(t *testing.T) {
	invalid := map[string]string{
		"two kinds":        strings.Replace(guardYAML, "  - not_exists:\n", "  - lock:\n      record: Member\n      name: $input.member\n    not_exists:\n", 1),
		"no where":         strings.Replace(guardYAML, "      where:\n        member: $input.member\n        status: Unpaid\n", "", 1),
		"alias on lock":    strings.Replace(guardYAML, "      name: $input.member\n  - exists:", "      name: $input.member\n    as: m\n  - exists:", 1),
		"item reference":   strings.Replace(guardYAML, "status: Unpaid", "status: $item.status", 1),
		"unknown alias":    strings.Replace(guardYAML, "name: $steps.loan.name", "name: $steps.fine.name", 1),
		"free-form string": strings.Replace(guardYAML, "require:", "preconditions:\n  - (> 1 0)\nrequire:", 1),
	}
	for name, yaml := range invalid {
		if _, err := ParseCommandResource([]byte(yaml)); err == nil {
			t.Fatalf("%s: definition was accepted", name)
		}
	}
}

const paymentGuardYAML = `name: pos.take_payment
version: 1
input:
  record: Order
require:
  - payment:
      for_each: $input.payments
      reference: $item.reference
      amount: $item.amount
      currency: $input.currency
transaction:
  - update:
      record: Order
      name: $input.order
      values:
        note: paid
`

func TestPaymentGuardParseAndValidation(t *testing.T) {
	def, err := ParseCommandResource([]byte(paymentGuardYAML))
	if err != nil || def.Require[0].Payment == nil || def.Require[0].Payment.Currency != "$input.currency" {
		t.Fatalf("parse: %v %#v", err, def)
	}
	cases := map[string]string{
		"unknown key":   strings.Replace(paymentGuardYAML, "currency: $input.currency", "currency: $input.currency\n      curency: x", 1),
		"missing field": strings.Replace(paymentGuardYAML, "      currency: $input.currency\n", "", 1),
		"bad for_each":  strings.Replace(paymentGuardYAML, "for_each: $input.payments", "for_each: payments", 1),
		"item currency": strings.Replace(paymentGuardYAML, "currency: $input.currency", "currency: $item.currency", 1),
		"literal ref":   strings.Replace(paymentGuardYAML, "reference: $item.reference", "reference: abc", 1),
		"no record":     strings.Replace(paymentGuardYAML, "input:\n  record: Order\n", "input: {}\n", 1),
		"nested ref":    strings.Replace(paymentGuardYAML, "amount: $item.amount", "amount: $item.a.b", 1),
		"two guards":    strings.Replace(paymentGuardYAML, "transaction:", "  - payment: {for_each: $input.p2, reference: $item.reference, amount: $item.amount, currency: $input.currency}\ntransaction:", 1),
	}
	for name, src := range cases {
		if _, err := ParseCommandResource([]byte(src)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

const equalGuardYAML = `name: pos.record_sale
version: 1
input:
  record: Sale
require:
  - equal: [$computed.paid, $computed.total]
    message: Payments must equal the sale total
compute:
  total: {sum: $input.items, expr: "quantity * unit_price"}
  paid: {sum: $input.payments, expr: "amount"}
transaction:
  - create:
      record: Sale
      values:
        register: $input.register
`

func TestEqualGuardParseAndValidation(t *testing.T) {
	def, err := ParseCommandResource([]byte(equalGuardYAML))
	if err != nil || len(def.Require) != 1 || len(def.Require[0].Equal) != 2 {
		t.Fatalf("parse: %v %#v", err, def)
	}
	invalid := map[string]string{
		"one operand":     strings.Replace(equalGuardYAML, "[$computed.paid, $computed.total]", "[$computed.paid]", 1),
		"three operands":  strings.Replace(equalGuardYAML, "[$computed.paid, $computed.total]", "[$computed.paid, $computed.total, 1]", 1),
		"unknown compute": strings.Replace(equalGuardYAML, "$computed.paid,", "$computed.nope,", 1),
		"item operand":    strings.Replace(equalGuardYAML, "$computed.paid,", "$item.amount,", 1),
		"steps operand":   strings.Replace(equalGuardYAML, "$computed.paid,", "$steps.sale.name,", 1),
		"non numeric":     strings.Replace(equalGuardYAML, "$computed.paid,", "abc,", 1),
		"two kinds":       strings.Replace(equalGuardYAML, "  - equal:", "  - lock: {record: Sale, name: $input.sale}\n    equal:", 1),
		"alias on equal":  strings.Replace(equalGuardYAML, "    message:", "    as: x\n    message:", 1),
	}
	for name, src := range invalid {
		if _, err := ParseCommandResource([]byte(src)); err == nil {
			t.Errorf("%s: definition was accepted", name)
		}
	}
}

// TestEqualGuardComparesComputedSums runs the compute block and the equality
// guard exactly as the executor does, over the published decimal rules.
func TestEqualGuardComparesComputedSums(t *testing.T) {
	def, err := ParseCommandResource([]byte(equalGuardYAML))
	if err != nil {
		t.Fatal(err)
	}
	line := func(qty, price any) map[string]any {
		return map[string]any{"quantity": qty, "unit_price": price}
	}
	pay := func(amount any) map[string]any { return map[string]any{"amount": amount} }
	cases := []struct {
		name     string
		items    []any
		payments []any
		wantErr  bool
	}{
		{"empty payments", []any{line(1.0, 65.0)}, []any{}, true},
		{"short", []any{line(2.0, 65.0)}, []any{pay(65.0)}, true},
		{"exact", []any{line(2.0, 65.0)}, []any{pay(130.0)}, false},
		{"exact split", []any{line(2.0, 65.0)}, []any{pay(100.0), pay(30.0)}, false},
		{"over", []any{line(1.0, 2.02)}, []any{pay(3.01)}, true},
		{"decimal exact", []any{line(3.0, 0.1)}, []any{pay(0.3)}, false},
		{"decimal string exact", []any{line(3.0, "0.1")}, []any{pay("0.30")}, false},
		{"one cent short", []any{line(1.0, 19.99)}, []any{pay(19.98)}, true},
		{"zero total no payments", []any{}, []any{}, false},
		{"zero total with payment", []any{}, []any{pay(1.0)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"items": tc.items, "payments": tc.payments}
			computed, err := evalCompute(def.Compute, input)
			if err != nil {
				t.Fatal(err)
			}
			cerr := checkEqualGuard(def.Require[0], refEnv{input: input, computed: computed}, 0)
			if (cerr != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr %v", cerr, tc.wantErr)
			}
			if cerr != nil && (cerr.Type != contract.CodeValidationFailed || cerr.Message != "Payments must equal the sale total") {
				t.Fatalf("error = %+v", cerr)
			}
		})
	}
}

func TestEqualGuardDefaultMessageAndBadOperand(t *testing.T) {
	g := CommandGuard{Equal: []string{"$input.a", "$input.b"}}
	env := refEnv{input: map[string]any{"a": "1", "b": "2"}}
	if cerr := checkEqualGuard(g, env, 3); cerr == nil || cerr.Type != contract.CodeValidationFailed || !strings.Contains(cerr.Message, "require 3") {
		t.Fatalf("default message: %+v", cerr)
	}
	env.input["b"] = "x"
	if cerr := checkEqualGuard(g, env, 3); cerr == nil || cerr.Type != contract.CodeValidationFailed {
		t.Fatalf("non numeric operand: %+v", cerr)
	}
}

func TestCommandGuardUnlessRoleValidation(t *testing.T) {
	withRole := func(extra string) string {
		return strings.Replace(guardYAML, "    message: Member has unpaid fines\n", "    message: Member has unpaid fines\n"+extra, 1)
	}
	tests := []struct {
		name string
		yaml string
		ok   bool
	}{
		{"not_exists with roles", withRole("    unless_role: [Owner, Manager]\n"), true},
		{"exists without alias", strings.Replace(guardYAML, "    as: loan\n", "    unless_role: [Owner]\n", 1), false},
		{"empty list", withRole("    unless_role: []\n"), false},
		{"empty role name", withRole("    unless_role: [Owner, \"\"]\n"), false},
		{"blank role name", withRole("    unless_role: [\" \"]\n"), false},
		{"not a list", withRole("    unless_role: Owner\n"), false},
		{"on lock", strings.Replace(guardYAML, "      name: $input.member\n  - exists:", "      name: $input.member\n    unless_role: [Owner]\n  - exists:", 1), false},
		{"on payment", strings.Replace(paymentGuardYAML, "      currency: $input.currency\n", "      currency: $input.currency\n    unless_role: [Owner]\n", 1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCommandResource([]byte(tc.yaml))
			if (err == nil) != tc.ok {
				t.Fatalf("accepted = %v, err = %v", err == nil, err)
			}
		})
	}
}

func TestCommandGuardSkippedForRoles(t *testing.T) {
	guard := CommandGuard{UnlessRole: []string{"Owner", "Manager"}}
	tests := []struct {
		name  string
		guard CommandGuard
		roles []string
		skip  bool
	}{
		{"listed role", guard, []string{"Manager"}, true},
		{"one of several roles", guard, []string{"Cashier", "Owner"}, true},
		{"unlisted role", guard, []string{"Cashier"}, false},
		{"exact match only", guard, []string{"manager", "Manager ", "Managers"}, false},
		{"no roles", guard, nil, false},
		{"no unless_role", CommandGuard{}, []string{"Owner"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.guard.skippedFor(tc.roles); got != tc.skip {
				t.Fatalf("skippedFor(%v) = %v, want %v", tc.roles, got, tc.skip)
			}
		})
	}
}
