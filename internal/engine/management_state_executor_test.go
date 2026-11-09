package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/asenawritescode/kora/internal/site"
)

type managementStateDirectoryStub struct {
	descriptor site.Descriptor
	expected   uint64
	state      string
	err        error
	calls      int
}

func (s *managementStateDirectoryStub) SetStatusAtRevision(_ context.Context, tenant, state string, expected uint64) (site.Descriptor, error) {
	s.calls++
	if tenant != s.descriptor.SiteID {
		return site.Descriptor{}, errors.New("wrong tenant")
	}
	s.state, s.expected = state, expected
	if s.err != nil {
		return site.Descriptor{}, s.err
	}
	s.descriptor.Status = state
	s.descriptor.ConfigRevision++
	return s.descriptor, nil
}

type managementStateCoordinatorStub struct {
	activated string
	retired   string
	err       error
}

func (s *managementStateCoordinatorStub) ActivateFromDirectory(_ context.Context, tenant string) error {
	s.activated = tenant
	return s.err
}
func (s *managementStateCoordinatorStub) Retire(_ context.Context, tenant string) error {
	s.retired = tenant
	return s.err
}

func TestManagementStateExecutorUsesRevisionFenceAndConvergesRuntime(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      contract.TenantState
		wantActive string
		wantRetire string
	}{
		{name: "suspend", state: contract.TenantSuspended, wantRetire: "tenant-a"},
		{name: "resume", state: contract.TenantActive, wantActive: "tenant-a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := &managementStateDirectoryStub{descriptor: site.Descriptor{SiteID: "tenant-a", ConfigRevision: 8}}
			coordinator := &managementStateCoordinatorStub{}
			executor, err := newManagementStateExecutor(directory, coordinator)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(contract.SetTenantState{
				Meta: contract.CommandMeta{IdempotencyKey: "state-operation-01"}, State: test.state,
				ExpectedConfigRevision: "7", Reason: "operator request",
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), managementTestClaim(management.Operation{
				ID: "operation-1", Tenant: "tenant-a", Kind: management.CommandSetTenantState, Command: payload,
			}))
			if err != nil || result.State != contract.OperationSucceeded || result.ObservedRevision != "9" {
				t.Fatalf("Execute = %+v, %v", result, err)
			}
			if directory.expected != 7 || directory.state != string(test.state) {
				t.Fatalf("directory transition state/revision = %q/%d", directory.state, directory.expected)
			}
			if coordinator.activated != test.wantActive || coordinator.retired != test.wantRetire {
				t.Fatalf("runtime convergence activated/retired = %q/%q", coordinator.activated, coordinator.retired)
			}
		})
	}
}

func TestManagementStateExecutorMakesConflictTerminalWithoutRuntimeEffect(t *testing.T) {
	directory := &managementStateDirectoryStub{descriptor: site.Descriptor{SiteID: "tenant-a"}, err: site.ErrSiteRevisionConflict}
	coordinator := &managementStateCoordinatorStub{}
	executor, err := newManagementStateExecutor(directory, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(contract.SetTenantState{
		Meta: contract.CommandMeta{IdempotencyKey: "state-operation-01"}, State: contract.TenantSuspended,
		ExpectedConfigRevision: "7", Reason: "operator request",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), managementTestClaim(management.Operation{Tenant: "tenant-a", Kind: management.CommandSetTenantState, Command: payload}))
	if err != nil || result.State != contract.OperationFailed || result.Error == nil || result.Error.Type != contract.CodeConflict {
		t.Fatalf("Execute conflict = %+v, %v", result, err)
	}
	if coordinator.retired != "" || coordinator.activated != "" {
		t.Fatal("revision conflict caused a runtime effect")
	}
}

func TestManagementStateExecutorReportsPersistedDirectoryStateWhenRuntimeRecoveryFails(t *testing.T) {
	directory := &managementStateDirectoryStub{descriptor: site.Descriptor{SiteID: "tenant-a", ConfigRevision: 8}}
	coordinator := &managementStateCoordinatorStub{err: errors.New("runtime drain failed")}
	executor, err := newManagementStateExecutor(directory, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(contract.SetTenantState{
		Meta: contract.CommandMeta{IdempotencyKey: "state-operation-01"}, State: contract.TenantSuspended,
		ExpectedConfigRevision: "7", Reason: "operator request",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), managementTestClaim(management.Operation{Tenant: "tenant-a", Kind: management.CommandSetTenantState, Command: payload}))
	if err != nil || result.State != contract.OperationRecoveryRequired || result.ObservedRevision != "9" || result.Error == nil {
		t.Fatalf("Execute recovery = %+v, %v", result, err)
	}
}

func managementTestClaim(operation management.Operation) management.Claim {
	return management.Claim{Operation: operation, Owner: "test-worker", Fence: 1}
}
