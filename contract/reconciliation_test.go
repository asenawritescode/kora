package contract

import (
	"strings"
	"testing"
)

func TestBusinessReconciliationContractsValidateBoundedIdentity(t *testing.T) {
	valid := BusinessReconciliationRunRequest{OwnerModuleID: "finance.reconciliation", RuleID: "invoices", RunID: "run-1"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid run request rejected: %v", err)
	}
	invalid := valid
	invalid.RunID = ""
	if err := invalid.Validate(); err == nil {
		t.Fatal("empty run ID was accepted")
	}
	invalid = valid
	invalid.RunID = strings.Repeat("a", 161)
	if err := invalid.Validate(); err == nil {
		t.Fatal("oversized run ID was accepted")
	}
	if err := (BusinessFindingDecision{FindingID: "finding-1"}).Validate(true); err == nil {
		t.Fatal("waiver without an audit reason was accepted")
	}
	if err := (BusinessFindingDecision{FindingID: "finding-1"}).Validate(false); err != nil {
		t.Fatalf("repair without an optional reason was rejected: %v", err)
	}
}
