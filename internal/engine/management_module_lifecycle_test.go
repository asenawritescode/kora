package engine

import (
	"context"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

type moduleLifecycleServiceInspector struct{}

func (moduleLifecycleServiceInspector) OpenWorkDomains() []string {
	return []string{"event_delivery", "domain_obligation"}
}

func (moduleLifecycleServiceInspector) InspectOpenWork(context.Context, contract.TenantID, contract.ModuleID, uint16) ([]contract.ModuleBlocker, error) {
	return []contract.ModuleBlocker{{Code: "domain_obligation_open", Count: 1}}, nil
}

func TestModuleLifecycleUsesOwnerInspectorsFromTenantServices(t *testing.T) {
	owner := contract.ModuleID("billing.core")
	inspector := moduleLifecycleServiceInspector{}
	executor := &managementModuleLifecycleExecutor{}
	inspectors := executor.moduleInspectors(owner, tenantruntime.Services{ModuleOpenWorkInspectors: []tenantruntime.ModuleOpenWorkInspector{{ModuleID: owner, Inspector: inspector}}})
	var found bool
	for _, candidate := range inspectors {
		coverage, ok := candidate.(contract.OpenWorkCoverageInspector)
		if !ok {
			continue
		}
		if len(coverage.OpenWorkDomains()) == 2 && coverage.OpenWorkDomains()[0] == "event_delivery" && coverage.OpenWorkDomains()[1] == "domain_obligation" {
			found = true
			blockers, err := candidate.InspectOpenWork(context.Background(), "tenant-a", owner, 10)
			if err != nil || len(blockers) != 1 || blockers[0].Code != "domain_obligation_open" {
				t.Fatalf("owner inspector was not invoked: blockers=%+v err=%v", blockers, err)
			}
		}
	}
	if !found {
		t.Fatal("tenant service owner inspector was not wired into lifecycle planning")
	}
}
