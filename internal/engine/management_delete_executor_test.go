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

type managementDeleteDirectoryStub struct {
	info       site.DBSiteInfo
	err        error
	deleting   bool
	deleteRev  uint64
	deleteData string
}

func (s *managementDeleteDirectoryStub) GetByIDContext(context.Context, string) (site.DBSiteInfo, error) {
	return s.info, s.err
}

func (s *managementDeleteDirectoryStub) SetDeletingAtAllocation(_ context.Context, tenant string, revision uint64, allocation string) (site.Descriptor, error) {
	s.deleting = true
	s.deleteRev = revision
	s.deleteData = allocation
	s.info.Status = "deleting"
	s.info.ConfigRevision++
	return site.Descriptor{SiteID: tenant, Status: "deleting"}, nil
}

type managementCheckpointVerifierStub struct {
	result VerifiedCheckpoint
	err    error
	tenant string
	data   string
	ref    string
	calls  int
}

func (s *managementCheckpointVerifierStub) Verify(_ context.Context, tenant, data, ref string) (VerifiedCheckpoint, error) {
	s.calls++
	s.tenant, s.data, s.ref = tenant, data, ref
	return s.result, s.err
}

type managementDeleteAdmissionStub struct {
	acquired int
	released int
	err      error
}

func (s *managementDeleteAdmissionStub) Acquire(context.Context) (func(), error) {
	s.acquired++
	if s.err != nil {
		return nil, s.err
	}
	return func() { s.released++ }, nil
}

func TestManagementDeleteExecutorRequiresVerifiedTenantAllocationCheckpoint(t *testing.T) {
	const tenantID = "site_0123456789abcdef0123456789abcdef"
	const checkpointRef = "checkpoint:backup-7"
	info := site.DBSiteInfo{SiteID: tenantID, Name: "tenant.example.test", DBType: "mysql", DBHost: "db.internal", DBPort: 3306, DBName: "tenant_db", FileStorage: "local"}
	allocation := site.DataPlaneReference(info)
	tests := []struct {
		name       string
		result     VerifiedCheckpoint
		verifyErr  error
		wantState  contract.OperationState
		wantDelete bool
	}{
		{
			name:      "valid checkpoint",
			result:    VerifiedCheckpoint{Reference: checkpointRef, TenantID: tenantID, DataPlaneRef: allocation, Complete: true, Immutable: true, Restorable: true},
			wantState: contract.OperationSucceeded, wantDelete: true,
		},
		{
			name:      "wrong tenant",
			result:    VerifiedCheckpoint{Reference: checkpointRef, TenantID: "another-tenant", DataPlaneRef: allocation, Complete: true, Immutable: true, Restorable: true},
			wantState: contract.OperationFailed,
		},
		{
			name:      "stale allocation",
			result:    VerifiedCheckpoint{Reference: checkpointRef, TenantID: tenantID, DataPlaneRef: "sha256:old-allocation", Complete: true, Immutable: true, Restorable: true},
			wantState: contract.OperationFailed,
		},
		{
			name:      "mutable checkpoint",
			result:    VerifiedCheckpoint{Reference: checkpointRef, TenantID: tenantID, DataPlaneRef: allocation, Complete: true, Restorable: true},
			wantState: contract.OperationFailed,
		},
		{
			name:      "provider outcome uncertain",
			verifyErr: errors.New("checkpoint provider unavailable"),
			wantState: contract.OperationRecoveryRequired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			admission := &managementDeleteAdmissionStub{}
			verifier := &managementCheckpointVerifierStub{result: test.result, err: test.verifyErr}
			deleteCalls := 0
			directory := &managementDeleteDirectoryStub{info: info}
			coordinator := &managementStateCoordinatorStub{}
			executor, err := newManagementDeleteExecutor(directory, verifier, coordinator, admission, func(_ context.Context, got site.DBSiteInfo) error {
				deleteCalls++
				if got.SiteID != tenantID || site.DataPlaneReference(got) != allocation {
					t.Errorf("deleter received unexpected tenant allocation: %+v", got)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			command, err := json.Marshal(contract.DeleteTenant{
				Meta: contract.CommandMeta{IdempotencyKey: "delete-operation-7"}, RecoveryCheckpointRef: checkpointRef,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(context.Background(), managementTestClaim(management.Operation{
				ID: "operation-7", Tenant: contract.TenantID(tenantID), Kind: management.CommandDeleteTenant, Command: command,
			}))
			if err != nil || result.State != test.wantState {
				t.Fatalf("Execute() = %+v, %v; want state %q", result, err, test.wantState)
			}
			if (deleteCalls == 1) != test.wantDelete {
				t.Fatalf("tenant delete calls = %d, want delete=%v", deleteCalls, test.wantDelete)
			}
			if directory.deleting != test.wantDelete || (directory.deleting && directory.deleteData != allocation) {
				t.Fatalf("allocation-fenced deleting transition = %v (%q)", directory.deleting, directory.deleteData)
			}
			if wantRetire := test.wantDelete; (coordinator.retired == tenantID) != wantRetire {
				t.Fatalf("tenant runtime retirement = %q, want retirement=%v", coordinator.retired, wantRetire)
			}
			if verifier.calls != 1 || verifier.tenant != tenantID || verifier.data != allocation || verifier.ref != checkpointRef {
				t.Fatalf("verifier calls/identity = %d/%q/%q/%q", verifier.calls, verifier.tenant, verifier.data, verifier.ref)
			}
			if admission.acquired != 1 || admission.released != 1 {
				t.Fatalf("admission acquired/released = %d/%d", admission.acquired, admission.released)
			}
		})
	}
}

func TestManagementDeleteExecutorFailsClosedWhenAdmissionOrDirectoryUnavailable(t *testing.T) {
	const tenantID = "site_0123456789abcdef0123456789abcdef"
	command, err := json.Marshal(contract.DeleteTenant{Meta: contract.CommandMeta{IdempotencyKey: "delete-operation-7"}, RecoveryCheckpointRef: "checkpoint:backup-7"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		directory *managementDeleteDirectoryStub
		admission *managementDeleteAdmissionStub
	}{
		{name: "directory unavailable", directory: &managementDeleteDirectoryStub{err: errors.New("directory offline")}, admission: &managementDeleteAdmissionStub{}},
		{name: "admission full", directory: &managementDeleteDirectoryStub{info: site.DBSiteInfo{SiteID: tenantID}}, admission: &managementDeleteAdmissionStub{err: errors.New("full")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			deleted := false
			executor, err := newManagementDeleteExecutor(test.directory, &managementCheckpointVerifierStub{}, &managementStateCoordinatorStub{}, test.admission, func(context.Context, site.DBSiteInfo) error { deleted = true; return nil })
			if err != nil {
				t.Fatal(err)
			}
			result, executeErr := executor.Execute(context.Background(), managementTestClaim(management.Operation{Tenant: contract.TenantID(tenantID), Kind: management.CommandDeleteTenant, Command: command}))
			if executeErr == nil && result.State != contract.OperationRecoveryRequired {
				t.Fatalf("Execute() = %+v, %v; want recovery-required", result, executeErr)
			}
			if deleted {
				t.Fatal("tenant deletion ran without a verified current allocation")
			}
		})
	}
}

func TestManagementDeleteExecutorDoesNotDropDatabaseWhenTenantDrainFails(t *testing.T) {
	const tenantID = "site_0123456789abcdef0123456789abcdef"
	const checkpointRef = "checkpoint:backup-7"
	info := site.DBSiteInfo{SiteID: tenantID, Name: "tenant.example.test", DBType: "mysql", DBHost: "db.internal", DBPort: 3306, DBName: "tenant_db", FileStorage: "local"}
	allocation := site.DataPlaneReference(info)
	directory := &managementDeleteDirectoryStub{info: info}
	verifier := &managementCheckpointVerifierStub{result: VerifiedCheckpoint{Reference: checkpointRef, TenantID: tenantID, DataPlaneRef: allocation, Complete: true, Immutable: true, Restorable: true}}
	coordinator := &managementStateCoordinatorStub{err: errors.New("tenant drain timed out")}
	deleted := false
	executor, err := newManagementDeleteExecutor(directory, verifier, coordinator, &managementDeleteAdmissionStub{}, func(context.Context, site.DBSiteInfo) error {
		deleted = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := json.Marshal(contract.DeleteTenant{Meta: contract.CommandMeta{IdempotencyKey: "delete-operation-7"}, RecoveryCheckpointRef: checkpointRef})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), managementTestClaim(management.Operation{
		ID: "operation-7", Tenant: contract.TenantID(tenantID), Kind: management.CommandDeleteTenant, Command: command,
	}))
	if err != nil || result.State != contract.OperationRecoveryRequired {
		t.Fatalf("Execute() = %+v, %v; want recovery-required", result, err)
	}
	if !directory.deleting || coordinator.retired != tenantID || deleted {
		t.Fatalf("directory deleting=%v, retired=%q, deleter called=%v", directory.deleting, coordinator.retired, deleted)
	}
}

func TestManagementDeleteExecutorRecoveryReverifiesCheckpointAndResumesFencedDelete(t *testing.T) {
	const tenantID = "site_0123456789abcdef0123456789abcdef"
	const checkpointRef = "checkpoint:backup-7"
	info := site.DBSiteInfo{SiteID: tenantID, Name: "tenant.example.test", DBType: "mysql", DBHost: "db.internal", DBPort: 3306, DBName: "tenant_db", FileStorage: "local"}
	allocation := site.DataPlaneReference(info)
	directory := &managementDeleteDirectoryStub{info: info}
	verifier := &managementCheckpointVerifierStub{result: VerifiedCheckpoint{Reference: checkpointRef, TenantID: tenantID, DataPlaneRef: allocation, Complete: true, Immutable: true, Restorable: true}}
	admission := &managementDeleteAdmissionStub{}
	deleteCalls := 0
	executor, err := newManagementDeleteExecutor(directory, verifier, &managementStateCoordinatorStub{}, admission, func(context.Context, site.DBSiteInfo) error {
		deleteCalls++
		if deleteCalls == 1 {
			return errors.New("tenant database provider timed out")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	command, err := json.Marshal(contract.DeleteTenant{Meta: contract.CommandMeta{IdempotencyKey: "delete-operation-7"}, RecoveryCheckpointRef: checkpointRef})
	if err != nil {
		t.Fatal(err)
	}
	operation := management.Operation{ID: "operation-7", Tenant: contract.TenantID(tenantID), Kind: management.CommandDeleteTenant, Command: command}
	first, err := executor.Execute(context.Background(), managementTestClaim(operation))
	if err != nil || first.State != contract.OperationRecoveryRequired {
		t.Fatalf("first Execute() = %+v, %v", first, err)
	}
	second, err := executor.Execute(context.Background(), managementTestClaim(operation))
	if err != nil || second.State != contract.OperationSucceeded {
		t.Fatalf("replayed Execute() = %+v, %v", second, err)
	}
	if verifier.calls != 2 || deleteCalls != 2 || !directory.deleting || directory.deleteData != allocation {
		t.Fatalf("verifications=%d deletes=%d directory deleting=%v allocation=%q", verifier.calls, deleteCalls, directory.deleting, directory.deleteData)
	}
	if admission.acquired != 2 || admission.released != 2 {
		t.Fatalf("admission acquired/released=%d/%d", admission.acquired, admission.released)
	}
}
