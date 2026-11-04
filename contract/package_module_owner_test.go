package contract

import "testing"

func TestScheduledJobOwnersMustResolveToDeclaredModules(t *testing.T) {
	modules := []ModuleDescriptor{{ID: "finance.billing", Version: "1.0.0"}}
	jobs := []ScheduledJobDefinition{{Name: "invoice-reconcile", OwnerModuleID: "finance.billing"}}
	if err := ValidateScheduledJobModuleOwners(jobs, modules); err != nil {
		t.Fatalf("declared scheduled job owner rejected: %v", err)
	}
	jobs[0].OwnerModuleID = "finance.unknown"
	if err := ValidateScheduledJobModuleOwners(jobs, modules); err == nil {
		t.Fatal("scheduled job with undeclared module owner accepted")
	}
}
