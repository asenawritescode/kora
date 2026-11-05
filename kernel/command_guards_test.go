package kernel

import (
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
