package management

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type fixedIDs struct{ id string }

func (f fixedIDs) NewID() string { return f.id }

type acceptStore struct {
	command        AcceptCommand
	err            error
	operation      Operation
	recoveryCaller contract.PrincipalID
	recoveryID     contract.OperationID
	recoveryKey    contract.IdempotencyKey
	recoveryReason string
}

func (s *acceptStore) Accept(_ context.Context, command AcceptCommand) (Operation, error) {
	s.command = command
	if s.err != nil {
		return Operation{}, s.err
	}
	return s.operation, nil
}
func (s *acceptStore) Get(context.Context, contract.PrincipalID, contract.OperationID) (Operation, error) {
	return s.operation, nil
}
func (s *acceptStore) ClaimDue(context.Context, string, uint16, time.Duration) ([]Claim, error) {
	return nil, nil
}
func (s *acceptStore) Renew(context.Context, Claim, time.Duration) (Claim, error) {
	return Claim{}, nil
}
func (s *acceptStore) Release(context.Context, Claim) error              { return nil }
func (s *acceptStore) Finish(context.Context, Claim, FinishResult) error { return nil }
func (s *acceptStore) RetryStateOperation(_ context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (Operation, error) {
	s.recoveryCaller, s.recoveryID, s.recoveryKey, s.recoveryReason = caller, id, key, reason
	return s.operation, s.err
}
func (s *acceptStore) RetryProvisionOperation(_ context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (Operation, error) {
	s.recoveryCaller, s.recoveryID, s.recoveryKey, s.recoveryReason = caller, id, key, reason
	return s.operation, s.err
}
func (s *acceptStore) RetryPackageOperation(_ context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (Operation, error) {
	s.recoveryCaller, s.recoveryID, s.recoveryKey, s.recoveryReason = caller, id, key, reason
	return s.operation, s.err
}
func (s *acceptStore) RetryDeleteOperation(_ context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (Operation, error) {
	s.recoveryCaller, s.recoveryID, s.recoveryKey, s.recoveryReason = caller, id, key, reason
	return s.operation, s.err
}

func TestServiceReconcilesValidatedTenantPackageOperationIdentity(t *testing.T) {
	store := &acceptStore{operation: Operation{ID: "01K7OPERATION000000000000000000", State: contract.OperationRequested}}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ReconcileTenantPackageOperation{
		Meta:   contract.CommandMeta{IdempotencyKey: "reconcile-package-1"},
		Reason: "operator inspected the durable package activation journal",
	}
	operation, err := service.ReconcileTenantPackage(context.Background(), "cloud-service", store.operation.ID, command)
	if err != nil || operation.ID != store.operation.ID {
		t.Fatalf("ReconcileTenantPackage() = %+v, %v", operation, err)
	}
	if store.recoveryCaller != "cloud-service" || store.recoveryID != operation.ID || store.recoveryKey != command.Meta.IdempotencyKey || store.recoveryReason != command.Reason {
		t.Fatalf("package reconciliation identity = caller %q operation %q key %q reason %q", store.recoveryCaller, store.recoveryID, store.recoveryKey, store.recoveryReason)
	}
}

func TestServiceAcceptUsesAuthenticatedCallerAndPersistsTypedCommand(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	store := &acceptStore{operation: Operation{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", State: contract.OperationRequested}}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ProvisionTenant{Meta: contract.CommandMeta{IdempotencyKey: "provision-key-1"}, Tenant: "tenant-1", BootstrapAdminEmail: "admin@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "tenant.example.test", Deployment: "deploy-1", DatabaseAllocationRef: "mysql:allocation-1"}
	if _, err := service.Accept(context.Background(), "cloud-service", command); err != nil {
		t.Fatal("Accept:", err)
	}
	if store.command.Caller != "cloud-service" || store.command.Tenant != command.Tenant || store.command.Kind != CommandProvisionTenant || store.command.Now != now || !jsonValid(store.command.Command) {
		t.Fatalf("accepted command identity/payload was not preserved: %+v", store.command)
	}
}

func TestServiceAcceptsTenantScopedModuleLifecycleCommand(t *testing.T) {
	store := &acceptStore{operation: Operation{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", State: contract.OperationRequested}}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ApplyModuleChange{Meta: contract.CommandMeta{IdempotencyKey: "module-change-01"}, ModuleID: "billing.core", TargetState: contract.ModuleDraining, PlanHash: strings.Repeat("a", 64), ExpectedRevision: 3}
	if _, err := service.AcceptForTenant(context.Background(), "cloud-service", "tenant-1", command); err != nil {
		t.Fatal(err)
	}
	if store.command.Tenant != "tenant-1" || store.command.Kind != CommandApplyModuleChange {
		t.Fatalf("accepted module lifecycle operation = %+v", store.command)
	}
	var stored contract.ApplyModuleChange
	if err := json.Unmarshal(store.command.Command, &stored); err != nil || stored.ModuleID != command.ModuleID || stored.TargetState != command.TargetState {
		t.Fatalf("stored module lifecycle command = %+v, %v", stored, err)
	}
}

func TestServiceDoesNotReturnAcceptedWhenDurableStoreFails(t *testing.T) {
	store := &acceptStore{err: errors.New("database unavailable")}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ProvisionTenant{Meta: contract.CommandMeta{IdempotencyKey: "provision-key-1"}, Tenant: "tenant-1", BootstrapAdminEmail: "admin@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "tenant.example.test", Deployment: "deploy-1", DatabaseAllocationRef: "mysql:allocation-1"}
	if operation, err := service.Accept(context.Background(), "cloud-service", command); err == nil || operation.ID != "" {
		t.Fatalf("Accept = %+v, %v; expected no accepted operation", operation, err)
	}
}

func TestServiceReconcilesOnlyValidatedTenantStateOperationIdentity(t *testing.T) {
	store := &acceptStore{operation: Operation{ID: "01K7OPERATION000000000000000000", State: contract.OperationRequested}}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ReconcileTenantStateOperation{
		Meta:   contract.CommandMeta{IdempotencyKey: "reconcile-state-1"},
		Reason: "verified current directory status before retry",
	}
	operation, err := service.ReconcileTenantState(context.Background(), "cloud-service", "01K7OPERATION000000000000000000", command)
	if err != nil || operation.ID != "01K7OPERATION000000000000000000" {
		t.Fatalf("ReconcileTenantState() = %+v, %v", operation, err)
	}
	if store.recoveryCaller != "cloud-service" || store.recoveryID != operation.ID || store.recoveryKey != command.Meta.IdempotencyKey || store.recoveryReason != command.Reason {
		t.Fatalf("reconciliation identity = caller %q operation %q key %q reason %q", store.recoveryCaller, store.recoveryID, store.recoveryKey, store.recoveryReason)
	}
	command.Reason = "  "
	if _, err := service.ReconcileTenantState(context.Background(), "cloud-service", operation.ID, command); err == nil {
		t.Fatal("accepted empty reconciliation reason")
	}
}

func TestServiceReconcilesValidatedBaseProvisionOperationIdentity(t *testing.T) {
	store := &acceptStore{operation: Operation{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", State: contract.OperationRequested}}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ReconcileTenantProvisionOperation{
		Meta:   contract.CommandMeta{IdempotencyKey: "reconcile-provision-1"},
		Reason: "verified tenant identity and allocation before replaying base bootstrap",
	}
	operation, err := service.ReconcileTenantProvision(context.Background(), "cloud-service", "01K7OPERATION000000000000000000", command)
	if err != nil || operation.ID != "01K7OPERATION000000000000000000" {
		t.Fatalf("ReconcileTenantProvision() = %+v, %v", operation, err)
	}
	if store.recoveryCaller != "cloud-service" || store.recoveryID != operation.ID || store.recoveryKey != command.Meta.IdempotencyKey || store.recoveryReason != command.Reason {
		t.Fatalf("provision reconciliation identity = caller %q operation %q key %q reason %q", store.recoveryCaller, store.recoveryID, store.recoveryKey, store.recoveryReason)
	}
	command.Reason = "  "
	if _, err := service.ReconcileTenantProvision(context.Background(), "cloud-service", operation.ID, command); err == nil {
		t.Fatal("accepted empty provisioning reconciliation reason")
	}
}

func TestServiceReconcilesValidatedTenantDeleteOperationIdentity(t *testing.T) {
	store := &acceptStore{operation: Operation{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", State: contract.OperationRequested}}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ReconcileTenantDeleteOperation{Meta: contract.CommandMeta{IdempotencyKey: "reconcile-delete-1"}, Reason: "verified checkpoint and allocation before retrying deletion"}
	operation, err := service.ReconcileTenantDelete(context.Background(), "cloud-service", "01K7OPERATION000000000000000000", command)
	if err != nil || operation.ID != "01K7OPERATION000000000000000000" {
		t.Fatalf("ReconcileTenantDelete() = %+v, %v", operation, err)
	}
	if store.recoveryCaller != "cloud-service" || store.recoveryID != operation.ID || store.recoveryKey != command.Meta.IdempotencyKey || store.recoveryReason != command.Reason {
		t.Fatalf("delete reconciliation identity = caller %q operation %q key %q reason %q", store.recoveryCaller, store.recoveryID, store.recoveryKey, store.recoveryReason)
	}
	command.Reason = " "
	if _, err := service.ReconcileTenantDelete(context.Background(), "cloud-service", operation.ID, command); err == nil {
		t.Fatal("accepted empty deletion reconciliation reason")
	}
}

func TestServiceRejectsInvalidCommandAndCallerBeforePersistence(t *testing.T) {
	store := &acceptStore{}
	service, err := NewService(store, fixedIDs{id: "01K7OPERATION000000000000000000"}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	command := contract.ProvisionTenant{Meta: contract.CommandMeta{IdempotencyKey: "provision-key-1"}, Tenant: "tenant-1", BootstrapAdminEmail: "admin@tenant.example.test", BootstrapAdminSubject: "usr_test_subject_1", PrimaryAlias: "tenant.example.test", Deployment: "deploy-1", DatabaseAllocationRef: "mysql://secret@host/db"}
	if _, err := service.Accept(context.Background(), "cloud-service", command); err == nil {
		t.Fatal("accepted unsafe command")
	}
	command.DatabaseAllocationRef = "mysql:allocation-1"
	if _, err := service.Accept(context.Background(), " ", command); err == nil {
		t.Fatal("accepted missing caller identity")
	}
	if store.command.ID != "" {
		t.Fatal("invalid command reached durable store")
	}
}

func TestCommandDigestIncludesKindAndCanonicalJSON(t *testing.T) {
	one := []byte(`{"tenant_id":"tenant-1","deployment_id":"deploy-1"}`)
	two := []byte(`{ "deployment_id": "deploy-1", "tenant_id": "tenant-1" }`)
	if CommandDigest(CommandProvisionTenant, one) != CommandDigest(CommandProvisionTenant, two) {
		t.Fatal("semantic JSON encoding changed digest")
	}
	if CommandDigest(CommandProvisionTenant, one) == CommandDigest(CommandApplyPackage, one) {
		t.Fatal("command kind was omitted from digest")
	}
}

func jsonValid(raw []byte) bool { return len(raw) > 0 && json.Valid(raw) }
