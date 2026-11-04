package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestReplaceModuleInstallationsPreservesLifecycleDuringPackageActivation(t *testing.T) {
	tests := []struct {
		name  string
		state contract.ModuleLifecycleState
	}{
		{name: "active module update", state: contract.ModuleActive},
		{name: "draining module update", state: contract.ModuleDraining},
		{name: "disabled module update", state: contract.ModuleDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceModuleInstallations([]contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "sales.order", Version: "1.0.0", ArtifactDigest: "old", State: string(tt.state), Revision: 8}}, "tenant-a", []contract.ModuleDescriptor{{ID: "sales.order", Version: "2.0.0"}}, "new")
			if len(got) != 1 || got[0].State != string(tt.state) || got[0].Revision != 9 || got[0].Version != "2.0.0" || got[0].ArtifactDigest != "new" {
				t.Fatalf("package activation changed lifecycle incorrectly: %+v", got)
			}
		})
	}
}

type lifecycleOpenWorkStub struct{ blockers []contract.ModuleBlocker }

func (s lifecycleOpenWorkStub) OpenWorkDomain() string { return "workflow_instance" }

func (s lifecycleOpenWorkStub) OpenWorkDomains() []string {
	return []string{"workflow_instance", "external_outcome", "scheduled_execution", "event_delivery", "domain_obligation"}
}

func (s lifecycleOpenWorkStub) InspectOpenWork(context.Context, contract.TenantID, contract.ModuleID, uint16) ([]contract.ModuleBlocker, error) {
	return s.blockers, nil
}

func TestPlanModuleLifecycleChangeDeterministicAndBlockedByDependentsAndWork(t *testing.T) {
	module := contract.ModuleDescriptor{ID: "billing.core", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: "billing.invoice", Contract: contract.SchemaRef{ID: "invoice", Version: 1}}}}
	dependent := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.invoice", Contract: contract.SchemaRef{ID: "invoice", Version: 1}, Required: true}}}
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 3, Modules: []contract.ModuleDescriptor{module, dependent}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, State: string(contract.ModuleActive), Revision: 2}}}
	request := contract.PlanModuleChange{ModuleID: module.ID, TargetState: contract.ModuleDraining, ExpectedRevision: 3}
	one, err := PlanModuleLifecycleChange(context.Background(), snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	two, err := PlanModuleLifecycleChange(context.Background(), snapshot, request)
	if err != nil || one.PlanHash != two.PlanHash {
		t.Fatalf("plan hash unstable: %q / %q, %v", one.PlanHash, two.PlanHash, err)
	}
	if len(one.DependencyBlockers) != 0 {
		t.Fatalf("drain unexpectedly blocked by dependent: %+v", one.DependencyBlockers)
	}

	snapshot.Installations[0].State = string(contract.ModuleDraining)
	request.TargetState = contract.ModuleDisabled
	blocked, err := PlanModuleLifecycleChange(context.Background(), snapshot, request, lifecycleOpenWorkStub{blockers: []contract.ModuleBlocker{{Code: "external_outcome_unknown", Count: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked.DependencyBlockers) != 1 || blocked.DependencyBlockers[0].Code != "required_by:sales.orders" {
		t.Fatalf("dependency blockers = %+v", blocked.DependencyBlockers)
	}
	if len(blocked.OpenWorkBlockers) != 1 || blocked.OpenWorkBlockers[0].Code != "external_outcome_unknown" {
		t.Fatalf("open-work blockers = %+v", blocked.OpenWorkBlockers)
	}
	request.ExpectedRevision++
	if _, err := PlanModuleLifecycleChange(context.Background(), snapshot, request); err == nil {
		t.Fatal("stale lifecycle plan request accepted")
	} else {
		var apiError *contract.Error
		if !errors.As(err, &apiError) || apiError.Type != contract.CodeConflict {
			t.Fatalf("stale lifecycle plan error = %v, want canonical conflict", err)
		}
	}
}

func TestPlanModuleLifecycleBlockersAreBoundedAggregatedAndDeterministic(t *testing.T) {
	module := contract.ModuleDescriptor{ID: "billing.core", Version: "1.0.0"}
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 1, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, State: string(contract.ModuleDraining)}}}
	request := contract.PlanModuleChange{ModuleID: module.ID, TargetState: contract.ModuleDisabled, ExpectedRevision: 1}
	a := lifecycleOpenWorkStub{blockers: []contract.ModuleBlocker{{Code: "workflow.open", Count: 2, Examples: []string{"wf-b", "wf-a"}}}}
	b := lifecycleOpenWorkStub{blockers: []contract.ModuleBlocker{{Code: "workflow.open", Count: 3, Examples: []string{"wf-c", "wf-a"}}}}
	first, err := PlanModuleLifecycleChange(context.Background(), snapshot, request, a, b)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanModuleLifecycleChange(context.Background(), snapshot, request, b, a)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanHash != second.PlanHash || len(first.OpenWorkBlockers) != 1 || first.OpenWorkBlockers[0].Count != 5 || len(first.OpenWorkBlockers[0].Examples) != 3 || first.OpenWorkBlockers[0].Examples[0] != "wf-a" {
		t.Fatalf("blockers are not stable and aggregated: first=%+v second=%+v", first, second)
	}
	bad := lifecycleOpenWorkStub{blockers: []contract.ModuleBlocker{{Code: "", Count: 1}}}
	if _, err := PlanModuleLifecycleChange(context.Background(), snapshot, request, bad); err == nil {
		t.Fatal("invalid owner blocker accepted")
	}
}

func TestPlanModuleLifecycleFailsClosedWhenOwnerWorkDomainsAreUninspected(t *testing.T) {
	module := contract.ModuleDescriptor{ID: "billing.core", Version: "1.0.0"}
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 1, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, State: string(contract.ModuleDraining)}}}
	request := contract.PlanModuleChange{ModuleID: module.ID, TargetState: contract.ModuleDisabled, ExpectedRevision: 1}
	partial := partialLifecycleOpenWorkStub{}
	plan, err := PlanModuleLifecycleChange(context.Background(), snapshot, request, partial)
	if err != nil {
		t.Fatal(err)
	}
	missing := map[string]bool{}
	for _, blocker := range plan.OpenWorkBlockers {
		missing[blocker.Code] = true
	}
	for _, domain := range []string{"domain_obligation", "event_delivery"} {
		if !missing["open_work_inspector_unavailable:"+domain] {
			t.Errorf("missing fail-closed blocker for %s: %+v", domain, plan.OpenWorkBlockers)
		}
	}
	if err := contract.ValidateModuleChangePlan(plan, snapshot.Revision, plan.OpenWorkBlockers); err == nil {
		t.Fatal("plan without complete owner work coverage was applicable")
	}
}

type partialLifecycleOpenWorkStub struct{}

func (partialLifecycleOpenWorkStub) OpenWorkDomain() string { return "workflow_instance" }
func (partialLifecycleOpenWorkStub) InspectOpenWork(context.Context, contract.TenantID, contract.ModuleID, uint16) ([]contract.ModuleBlocker, error) {
	return nil, nil
}

type lifecycleOpenWorkCallback struct {
	inspect func() []contract.ModuleBlocker
}

func (lifecycleOpenWorkCallback) OpenWorkDomains() []string {
	return []string{"domain_obligation", "event_delivery", "external_outcome", "scheduled_execution", "workflow_instance"}
}
func (s lifecycleOpenWorkCallback) InspectOpenWork(context.Context, contract.TenantID, contract.ModuleID, uint16) ([]contract.ModuleBlocker, error) {
	return s.inspect(), nil
}

func TestApplyModuleLifecycleRejectsPlanWhenOpenWorkAppears(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	module := contract.ModuleDescriptor{ID: "billing.core", Version: "1.0.0"}
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 4, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, State: string(contract.ModuleDraining)}}}
	request := contract.PlanModuleChange{ModuleID: module.ID, TargetState: contract.ModuleDisabled, ExpectedRevision: snapshot.Revision}
	clean := lifecycleOpenWorkStub{}
	plan, err := PlanModuleLifecycleChange(context.Background(), snapshot, request, clean)
	if err != nil || len(plan.OpenWorkBlockers) != 0 {
		t.Fatalf("initial plan = %+v, %v", plan, err)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?")).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = \?`).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(snapshot.Revision, string(payload)))
	inspectedAfterFence := false
	newWork := lifecycleOpenWorkCallback{inspect: func() []contract.ModuleBlocker {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("open work inspected before lifecycle row fence and snapshot read: %v", err)
		} else {
			inspectedAfterFence = true
		}
		return []contract.ModuleBlocker{{Code: "domain_obligation_open", Count: 1}}
	}}
	_, err = NewModuleStore(database, db.Resolve("libsql")).ApplyModuleLifecycleChangeTx(context.Background(), tx, "tenant-a", "operation-1", snapshot.Revision, plan, newWork)
	if err == nil {
		t.Fatal("apply accepted after new open work appeared")
	}
	if !inspectedAfterFence {
		t.Fatal("apply did not acquire lifecycle row fence before open-work inspection")
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleLifecycleDrainStateSurvivesStoreReload(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	drainedAt := time.Date(2026, 11, 3, 9, 15, 0, 0, time.UTC)
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 8, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "billing.core", Version: "1.0.0", ArtifactDigest: "artifact", State: string(contract.ModuleDraining), Revision: 5, StateChangedAt: drainedAt, LastLifecycleOperationID: "operation-drain-8"}}}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = \?`).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(snapshot.Revision, string(payload)))
	reloaded, err := NewModuleStore(database, db.Resolve("libsql")).Load("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	installation := reloaded.Installations[0]
	if installation.State != string(contract.ModuleDraining) || !installation.StateChangedAt.Equal(drainedAt) || installation.LastLifecycleOperationID != "operation-drain-8" {
		t.Fatalf("reloaded lifecycle state lost drain recovery metadata: %+v", installation)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanModuleLifecycleProviderCutoverIsDeterministic(t *testing.T) {
	module := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.invoice", Contract: contract.SchemaRef{ID: "invoice", Version: 1}, Required: true}, {Name: "tax.quote", Contract: contract.SchemaRef{ID: "tax", Version: 1}, Required: true}}}
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 5, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, State: string(contract.ModuleActive)}}}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	first := contract.ProviderSelection{Key: contract.BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: "billing.invoice"}, ProviderID: "billing.external"}
	second := contract.ProviderSelection{Key: contract.BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: "tax.quote"}, ProviderID: "tax.external"}
	request := contract.PlanModuleChange{ModuleID: module.ID, TargetState: contract.ModuleActive, ProviderSelections: []contract.ProviderSelection{first, second}, ExpectedRevision: 5}
	a, err := PlanModuleLifecycleChange(context.Background(), snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	request.ProviderSelections = []contract.ProviderSelection{second, first}
	b, err := PlanModuleLifecycleChange(context.Background(), snapshot, request)
	if err != nil {
		t.Fatal(err)
	}
	if a.PlanHash != b.PlanHash || len(a.BindingChanges) != 2 {
		t.Fatalf("provider cutover plan unstable: %+v / %+v", a, b)
	}
}

func TestModuleStoreActivationIsAtomicAndRevisionFenced(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, db.Resolve("libsql"))
	resource := contract.OwnedResource{Ref: contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}, Access: contract.ModulePrivate, MutationMode: contract.MutationCommands, ScopeKind: contract.ScopeTenantShared}
	candidate := contract.ModuleSnapshot{TenantID: "tenant-a", Modules: []contract.ModuleDescriptor{{ID: "finance.ar", Version: "1.0.0", Resources: []contract.OwnedResource{resource}}}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", Version: "1.0.0", ArtifactDigest: "digest"}}, Ownership: []contract.ResourceOwnership{{TenantID: "tenant-a", Resource: resource.Ref, OwnerModuleID: "finance.ar"}}}
	query := regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(query).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)`)).WithArgs("tenant-a", uint64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	if revision, err := store.CommitActivation(context.Background(), tx, 0, candidate); err != nil || revision != 1 {
		t.Fatalf("revision=%d err=%v", revision, err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseAdmissionAndRevocationAreEpochFenced(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-1", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "finance.billing", Operation: "invoice.issue", BindingRevision: 4, GrantEpoch: 3, ExpiresAt: now.Add(time.Hour), State: "active"}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_capability_lease (tenant_id, lease_id, scope_json, caller_module_id, provider_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref, revoked_by, revocation_reason, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)).WithArgs("tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "finance.billing", "finance.billing", "invoice.issue", uint64(4), uint64(3), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "active", uint64(0), "", "", "", "").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.CreateCapabilityLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	snapshotJSON, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 1, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "sales.orders", State: "active"}, {TenantID: "tenant-a", ModuleID: "finance.billing", State: "active"}}})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(1), string(snapshotJSON)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET admitted_attempts = admitted_attempts + 1 WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at > ? AND credential_ref = ?`)).WithArgs("tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "finance.billing", "finance.billing", "invoice.issue", uint64(4), uint64(3), now.Format(capabilityLeaseTimeLayout), "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.AdmitCapabilityAttempt(context.Background(), lease, now); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active'`)).WithArgs("security-admin", "credential compromised", now.Format(capabilityLeaseTimeLayout), "tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "finance.billing", "invoice.issue", uint64(4), uint64(3)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.RevokeCapabilityLease(context.Background(), lease, "security-admin", "credential compromised", now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseAdmissionRejectsDrainingCaller(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-draining", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 2, GrantEpoch: 1, ExpiresAt: now.Add(time.Hour), State: "active"}
	snapshotJSON, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 2, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "sales.orders", State: "draining"}}})
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(2), string(snapshotJSON)))
	mock.ExpectRollback()
	if err := store.AdmitCapabilityAttempt(context.Background(), lease, now); err == nil {
		t.Fatal("draining module capability attempt was admitted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedCapabilityRecoveryAdmissionAllowsOnlyPreDrainAttempt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	started := now.Add(-time.Minute)
	drained := now.Add(-30 * time.Second)
	lease := contract.CapabilityLease{ID: "lease-recovery", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 2, GrantEpoch: 1, ExpiresAt: now.Add(time.Hour), State: "active"}
	dispatch := contract.PinnedDispatch{ExecutionID: "execution-a", Scope: lease.Scope, CallerModuleID: lease.CallerModuleID, ProviderModuleID: lease.ProviderModuleID, ProviderID: lease.ProviderID, BindingRevision: lease.BindingRevision, Operation: contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "issue", InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}}, IdempotencyKey: "idem-a", InputHash: strings.Repeat("a", 64)}
	pinnedJSON, _ := json.Marshal(struct {
		Dispatch contract.PinnedDispatch
		Lease    contract.CapabilityLease
	}{dispatch, lease})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pinned_dispatch_json,started_at FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND step_id=? AND execution_id=? AND pinned_dispatch_json IS NOT NULL ORDER BY attempt LIMIT 1`)).WithArgs("tenant-a", "instance-a", "step-a", "execution-a").WillReturnRows(sqlmock.NewRows([]string{"pinned_dispatch_json", "started_at"}).AddRow(string(pinnedJSON), started))
	snapshotJSON, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 2, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "sales.orders", State: "active"}, {TenantID: "tenant-a", ModuleID: "finance.billing", State: "draining", StateChangedAt: drained}}})
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(2), string(snapshotJSON)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET admitted_attempts = admitted_attempts + 1 WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at > ? AND credential_ref = ?`)).WithArgs("tenant-a", "lease-recovery", sqlmock.AnyArg(), "sales.orders", "finance.billing", "billing", "issue", uint64(2), uint64(1), now.Format(capabilityLeaseTimeLayout), "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.AdmitPinnedCapabilityRecovery(context.Background(), dispatch, lease, "tenant-a", "instance-a", "step-a", now); err != nil {
		t.Fatalf("pre-drain retry denied: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedCapabilityRecoveryAdmissionRejectsPostDrainAttempt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-recovery-late", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 2, GrantEpoch: 1, ExpiresAt: now.Add(time.Hour), State: "active"}
	dispatch := contract.PinnedDispatch{ExecutionID: "execution-late", Scope: lease.Scope, CallerModuleID: lease.CallerModuleID, ProviderModuleID: lease.ProviderModuleID, ProviderID: lease.ProviderID, BindingRevision: lease.BindingRevision, Operation: contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "issue", InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}}, IdempotencyKey: "idem-late", InputHash: strings.Repeat("b", 64)}
	pinnedJSON, _ := json.Marshal(struct {
		Dispatch contract.PinnedDispatch
		Lease    contract.CapabilityLease
	}{dispatch, lease})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pinned_dispatch_json,started_at FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND step_id=? AND execution_id=? AND pinned_dispatch_json IS NOT NULL ORDER BY attempt LIMIT 1`)).WithArgs("tenant-a", "instance-late", "step-late", "execution-late").WillReturnRows(sqlmock.NewRows([]string{"pinned_dispatch_json", "started_at"}).AddRow(string(pinnedJSON), now))
	drained := now.Add(-time.Minute)
	snapshotJSON, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 3, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "sales.orders", State: "active"}, {TenantID: "tenant-a", ModuleID: "finance.billing", State: "draining", StateChangedAt: drained}}})
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(3), string(snapshotJSON)))
	mock.ExpectRollback()
	if err := store.AdmitPinnedCapabilityRecovery(context.Background(), dispatch, lease, "tenant-a", "instance-late", "step-late", now); err == nil {
		t.Fatal("post-drain attempt was admitted as recovery")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeCapabilitiesIsScopeAndRevisionFenced(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	request := contract.RevokeCapability{Scope: scope, Selector: contract.CapabilitySelector{CallerModuleID: "sales.orders", ProviderID: "billing.provider", Operation: "issue"}, ExpectedRevision: 4, Reason: "provider credential compromised"}
	scopeJSON, _ := json.Marshal(scope)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND state = 'active'`)).WithArgs("security-admin", request.Reason, now.Format(capabilityLeaseTimeLayout), "tenant-a", string(scopeJSON), "sales.orders", "billing.provider", "issue", uint64(4)).WillReturnResult(sqlmock.NewResult(0, 2))
	count, err := store.RevokeCapabilities(context.Background(), request, "security-admin", now)
	if err != nil || count != 2 {
		t.Fatalf("scope revocation = %d, %v; want two active matching leases", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseSchemaUsesTransactionalMySQLTable(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	mock.ExpectExec(regexp.QuoteMeta(`CREATE TABLE IF NOT EXISTS _kora_capability_lease (tenant_id VARCHAR(140) NOT NULL, lease_id VARCHAR(160) NOT NULL, scope_json TEXT NOT NULL, caller_module_id VARCHAR(160) NOT NULL, provider_module_id VARCHAR(160) NOT NULL, provider_id VARCHAR(160) NOT NULL, operation_name VARCHAR(160) NOT NULL, binding_revision BIGINT NOT NULL, grant_epoch BIGINT NOT NULL, expires_at VARCHAR(40) NOT NULL, state VARCHAR(16) NOT NULL, admitted_attempts BIGINT NOT NULL, credential_ref VARCHAR(320) NOT NULL, revoked_by VARCHAR(160) NOT NULL, revocation_reason VARCHAR(1000) NOT NULL, revoked_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, lease_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`CREATE TABLE IF NOT EXISTS _kora_published_operation_receipt (tenant_id VARCHAR(140) NOT NULL, execution_id VARCHAR(160) NOT NULL, actor_type VARCHAR(40) NOT NULL, actor_id VARCHAR(160) NOT NULL, intent_hash VARCHAR(64) NOT NULL, input_hash VARCHAR(64) NOT NULL, state VARCHAR(32) NOT NULL, dispatch_state VARCHAR(24) NOT NULL, operation_ref VARCHAR(256) NOT NULL, scope_json TEXT NOT NULL, dispatch_json TEXT NOT NULL, lease_json TEXT NOT NULL, receipt_json TEXT NOT NULL, PRIMARY KEY (tenant_id, execution_id), UNIQUE (tenant_id, actor_type, actor_id, intent_hash)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.EnsureCapabilityLeaseSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseValidationRejectsStaleOrExpiredGrant(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.LibSQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-2", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 8, GrantEpoch: 2, ExpiresAt: now.Add(time.Hour), State: "active"}
	query := regexp.QuoteMeta(`SELECT scope_json, caller_module_id, provider_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`)
	scopeJSON, _ := json.Marshal(lease.Scope)
	mock.ExpectQuery(query).WithArgs("tenant-a", "lease-2").WillReturnRows(sqlmock.NewRows([]string{"scope_json", "caller_module_id", "provider_module_id", "provider_id", "operation_name", "binding_revision", "grant_epoch", "expires_at", "state", "credential_ref"}).AddRow(string(scopeJSON), "sales.orders", "finance.billing", "billing", "issue", uint64(8), uint64(2), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "active", ""))
	if err := store.ValidateCapabilityLease(context.Background(), lease, now); err != nil {
		t.Fatalf("valid durable lease rejected: %v", err)
	}
	lease.GrantEpoch++
	mock.ExpectQuery(query).WithArgs("tenant-a", "lease-2").WillReturnRows(sqlmock.NewRows([]string{"scope_json", "caller_module_id", "provider_module_id", "provider_id", "operation_name", "binding_revision", "grant_epoch", "expires_at", "state", "credential_ref"}).AddRow(string(scopeJSON), "sales.orders", "finance.billing", "billing", "issue", uint64(8), uint64(2), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "active", ""))
	if err := store.ValidateCapabilityLease(context.Background(), lease, now); err == nil {
		t.Fatal("stale grant epoch accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCapabilityLeasePreservesRevokedGrantForRecovery(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.LibSQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-3", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 2, GrantEpoch: 4, ExpiresAt: now.Add(time.Hour), State: "active"}
	query := regexp.QuoteMeta(`SELECT scope_json, caller_module_id, provider_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`)
	scopeJSON, _ := json.Marshal(lease.Scope)
	mock.ExpectQuery(query).WithArgs("tenant-a", "lease-3").WillReturnRows(sqlmock.NewRows([]string{"scope_json", "caller_module_id", "provider_module_id", "provider_id", "operation_name", "binding_revision", "grant_epoch", "expires_at", "state", "admitted_attempts", "credential_ref"}).AddRow(string(scopeJSON), "sales.orders", "finance.billing", "billing", "issue", uint64(2), uint64(5), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "revoked", uint64(7), ""))
	if err := store.EnsureCapabilityLease(context.Background(), lease); err != nil {
		t.Fatalf("revoked lease should not prevent reconciliation runtime startup: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseAdmissionPersistsExpiry(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.LibSQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-expired", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 2, GrantEpoch: 1, ExpiresAt: now.Add(-time.Second), State: "active"}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`)).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	snapshotJSON, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 1, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "sales.orders", State: "active"}, {TenantID: "tenant-a", ModuleID: "finance.billing", State: "active"}}})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(1), string(snapshotJSON)))
	if err := store.AdmitCapabilityAttempt(context.Background(), lease, now); err == nil {
		t.Fatal("expired capability lease admitted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseRejectsInvalidScopeAndStaleRevocation(t *testing.T) {
	invalid := contract.CapabilityLease{ID: "lease-1", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-b", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 1, GrantEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), State: "active"}
	if err := invalid.Validate(); err == nil {
		t.Fatal("cross-tenant capability lease accepted")
	}
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	stale := contract.CapabilityLease{ID: "lease-1", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing", Operation: "issue", BindingRevision: 1, GrantEpoch: 2, ExpiresAt: time.Now().Add(time.Hour), State: "active"}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active'`)).WithArgs("admin", "revoke", sqlmock.AnyArg(), "tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "billing", "issue", uint64(1), uint64(2)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.RevokeCapabilityLease(context.Background(), stale, "admin", "revoke", time.Now().UTC()); err == nil {
		t.Fatal("stale revocation accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleStoreProviderSelectionsAreRevisioned(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, db.Resolve("libsql"))
	module := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 1}, Required: true}}}
	current := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 1, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, ArtifactDigest: "artifact"}}}
	currentJSON, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	selection := contract.ProviderSelection{Key: contract.BindingKey{Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, ConsumerModuleID: module.ID, CapabilityName: "billing.issue"}, ProviderID: "billing-internal"}
	selectQuery := regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)
	mock.ExpectBegin()
	mock.ExpectQuery(selectQuery).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(1), string(currentJSON)))
	mock.ExpectQuery(selectQuery).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(1), string(currentJSON)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision = ?, snapshot_json = ? WHERE tenant_id = ? AND revision = ?`)).WithArgs(uint64(2), sqlmock.AnyArg(), "tenant-a", uint64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	updated, err := store.CommitProviderSelections(context.Background(), "tenant-a", 1, []contract.ProviderSelection{selection})
	if err != nil || updated.Revision != 2 || len(updated.ProviderSelections) != 1 || updated.ProviderSelections[0].ProviderID != "billing-internal" {
		t.Fatalf("persist provider selection = %+v, %v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateModuleSnapshotRequiresSelectionsToMatchInstalledCapabilities(t *testing.T) {
	module := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 1}, Required: true}}}
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, ArtifactDigest: "artifact"}}, ProviderSelections: []contract.ProviderSelection{{Key: contract.BindingKey{Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, ConsumerModuleID: module.ID, CapabilityName: "billing.issue"}, ProviderID: "billing.external"}}}
	if err := validateModuleSnapshot(snapshot); err != nil {
		t.Fatalf("valid persisted provider selection: %v", err)
	}
	snapshot.ProviderSelections = append(snapshot.ProviderSelections, snapshot.ProviderSelections[0])
	if err := validateModuleSnapshot(snapshot); err == nil {
		t.Fatal("duplicate persisted provider selection accepted")
	}
}

func TestValidateModuleSnapshotRejectsInvalidInstalledPackageComposition(t *testing.T) {
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Packages: []contract.InstalledPackage{
		{PackageID: "sales", ReleaseID: "sales-1", Version: "1.0.0", Digest: "sales-digest", Requires: []contract.ManifestPackRef{{PackageID: "billing", ReleaseID: "billing-1"}}},
	}}
	if err := validateModuleSnapshot(snapshot); err == nil {
		t.Fatal("module snapshot with missing package requirement accepted")
	}
}

func TestModuleStoreSchemaUsesSupportedDialectTypes(t *testing.T) {
	for _, name := range []string{"mysql", "postgres", "libsql"} {
		statement := NewModuleStore(nil, db.Resolve(name)).SchemaStatement()
		for _, required := range []string{"_kora_module_snapshot", "tenant_id", "revision", "snapshot_json"} {
			if !strings.Contains(statement, required) {
				t.Fatalf("%s schema missing %s: %s", name, required, statement)
			}
		}
		if name == "mysql" && !strings.Contains(statement, "LONGTEXT") {
			t.Fatalf("mysql snapshot must use LONGTEXT: %s", statement)
		}
	}
}

var _ *sql.DB

func TestPublishedOperationReceiptLookupBindsActorAndExactScope(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	actor := contract.ActorContext{PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-a"}
	receipt := contract.OperationReceipt{ExecutionID: "execution-a", OperationRef: "finance.billing~billing.invoice~issue", BusinessScope: scope, Status: contract.ReceiptRunning, Revision: "7"}
	scopeJSON, _ := json.Marshal(scope)
	receiptJSON, _ := json.Marshal(receipt)
	input := []byte(`{"amount":3}`)
	inputHash := PublishedOperationInputHash(input)
	intentHash := strings.Repeat("a", 64)
	inputSchema, outputSchema := contract.SchemaRef{ID: "billing.input", Version: 1}, contract.SchemaRef{ID: "billing.output", Version: 1}
	lease := contract.CapabilityLease{ID: "lease-a", TenantID: scope.TenantID, Scope: scope, CallerModuleID: "sales.orders", ProviderModuleID: "finance.billing", ProviderID: "billing-provider", Operation: "issue", BindingRevision: 4, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	dispatch := contract.PinnedDispatch{ExecutionID: string(receipt.ExecutionID), Scope: scope, CallerModuleID: lease.CallerModuleID, ProviderModuleID: lease.ProviderModuleID, ProviderID: lease.ProviderID, BindingRevision: lease.BindingRevision, Operation: contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "issue", InputSchema: inputSchema, OutputSchema: outputSchema}, IdempotencyKey: contract.IdempotencyKey("published:" + intentHash), InputHash: inputHash}
	dispatchJSON, _ := json.Marshal(&dispatch)
	leaseJSON, _ := json.Marshal(&lease)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_published_operation_receipt (tenant_id,execution_id,actor_type,actor_id,intent_hash,input_hash,state,dispatch_state,operation_ref,scope_json,dispatch_json,lease_json,receipt_json) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)).WithArgs("tenant-a", "execution-a", "human", "alice", intentHash, inputHash, "running", "claimed", receipt.OperationRef, string(scopeJSON), string(dispatchJSON), string(leaseJSON), string(receiptJSON)).WillReturnResult(sqlmock.NewResult(1, 1))
	attempt, created, err := store.BeginPublishedOperationAttempt(context.Background(), actor, PublishedOperationAttempt{Receipt: receipt, IntentHash: intentHash, InputHash: inputHash, Dispatch: &dispatch, Lease: &lease})
	if err != nil || !created || attempt.Receipt.ExecutionID != receipt.ExecutionID {
		t.Fatalf("begin operation attempt=(%+v,%v,%v)", attempt, created, err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT execution_id,operation_ref,scope_json,input_hash,state,dispatch_state,dispatch_json,lease_json,receipt_json FROM _kora_published_operation_receipt WHERE tenant_id=? AND intent_hash=? AND actor_type=? AND actor_id=?`)).WithArgs("tenant-a", intentHash, "human", "alice").WillReturnRows(sqlmock.NewRows([]string{"execution_id", "operation_ref", "scope_json", "input_hash", "state", "dispatch_state", "dispatch_json", "lease_json", "receipt_json"}).AddRow("execution-a", receipt.OperationRef, string(scopeJSON), inputHash, "running", "claimed", string(dispatchJSON), string(leaseJSON), string(receiptJSON)))
	loaded, found, err := store.LoadPublishedOperationAttempt(context.Background(), scope.TenantID, intentHash, actor, scope)
	if err != nil || !found || loaded.Receipt.OperationRef != receipt.OperationRef || loaded.InputHash != inputHash {
		t.Fatalf("intent lookup=(%+v,%v,%v)", loaded, found, err)
	}
	other := scope
	other.LegalEntityID = "entity-b"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT execution_id,operation_ref,scope_json,input_hash,state,dispatch_state,dispatch_json,lease_json,receipt_json FROM _kora_published_operation_receipt WHERE tenant_id=? AND intent_hash=? AND actor_type=? AND actor_id=?`)).WithArgs("tenant-a", intentHash, "human", "alice").WillReturnRows(sqlmock.NewRows([]string{"execution_id", "operation_ref", "scope_json", "input_hash", "state", "dispatch_state", "dispatch_json", "lease_json", "receipt_json"}).AddRow("execution-a", receipt.OperationRef, string(scopeJSON), inputHash, "running", "claimed", string(dispatchJSON), string(leaseJSON), string(receiptJSON)))
	if _, found, err := store.LoadPublishedOperationAttempt(context.Background(), scope.TenantID, intentHash, actor, other); err == nil || found {
		t.Fatalf("cross-entity receipt lookup=(found %v, err %v)", found, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
