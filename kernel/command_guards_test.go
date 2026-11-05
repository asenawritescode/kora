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
