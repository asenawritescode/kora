package doctype

import "testing"

// erpsellPackDir is the selling pack as shipped. It is parked, so the skip
// helper used by the other parked-pack tests would hide it; this test does not skip.
const erpsellPackDir = "../../config/_parked/erpsell"

func TestErpsellDoctypesParseAndLinkCleanly(t *testing.T) {
	doctypes, err := ParseConfigTree(erpsellPackDir)
	if err != nil {
		t.Fatalf("parse erpsell: %v", err)
	}
	for _, problem := range ValidateAll(doctypes) {
		t.Errorf("%s", problem.Error())
	}
}
