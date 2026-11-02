//go:build integration

package configstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

type lifecycleIntegrationInspector struct{}

func (lifecycleIntegrationInspector) OpenWorkDomains() []string {
	return []string{"domain_obligation", "event_delivery", "external_outcome", "scheduled_execution", "workflow_instance"}
}

func (lifecycleIntegrationInspector) InspectOpenWork(context.Context, contract.TenantID, contract.ModuleID, uint16) ([]contract.ModuleBlocker, error) {
	return nil, nil
}

func TestModuleLifecycleDrainAndDisablePersistAcrossDialects(t *testing.T) {
	tests := []struct {
		name, env, driver string
		dialect           func() db.Dialect
	}{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN", "mysql", func() db.Dialect { return db.Resolve("mysql") }},
		{"postgres", "KORA_API_LIVE_POSTGRES_DSN", "postgres", func() db.Dialect { return db.Resolve("postgres") }},
		{"libsql", "KORA_CONFIG_DISPOSABLE_LIBSQL_DSN", "libsql", func() db.Dialect { return db.Resolve("libsql") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable loopback SQL service", test.env)
			}
			database, cleanup := openFenceTestDatabase(t, test.driver, dsn)
			defer cleanup()
			store := configstore.NewModuleStore(database, test.dialect())
			if err := store.EnsureSchema(); err != nil {
				t.Fatal("ensure module lifecycle schema:", err)
			}
			ctx := context.Background()
			tenant := contract.TenantID(fmt.Sprintf("tenant-lifecycle-%s-%d", test.name, time.Now().UnixNano()))
			module := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0"}
			snapshot := contract.ModuleSnapshot{
				TenantID: tenant,
				Modules:  []contract.ModuleDescriptor{module},
				Installations: []contract.ModuleInstallation{{
					TenantID: tenant, ModuleID: module.ID, Version: module.Version,
					ArtifactDigest: "fixture-digest", State: string(contract.ModuleActive), Revision: 1,
				}},
			}
			tx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin module snapshot seed:", err)
			}
			if _, err := store.CommitActivation(ctx, tx, 0, snapshot); err != nil {
				_ = tx.Rollback()
				t.Fatal("seed active module snapshot:", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal("commit active module snapshot:", err)
			}

			active, err := store.Load(tenant)
			if err != nil {
				t.Fatal("load active module snapshot:", err)
			}
			drainRequest := contract.PlanModuleChange{ModuleID: module.ID, TargetState: contract.ModuleDraining, ExpectedRevision: active.Revision}
			drainPlan, err := configstore.PlanModuleLifecycleChange(ctx, active, drainRequest)
			if err != nil {
				t.Fatal("plan module drain:", err)
			}
			leaseNow := time.Now().UTC()
			lease := contract.CapabilityLease{
				ID: "lease-lifecycle-" + test.name, TenantID: tenant,
				Scope:          contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant},
				CallerModuleID: module.ID, ProviderModuleID: module.ID, ProviderID: "billing.provider",
				Operation: "issue", BindingRevision: 1, GrantEpoch: 1,
				ExpiresAt: leaseNow.Add(time.Hour), State: "active",
			}
			if err := store.CreateCapabilityLease(ctx, lease); err != nil {
				t.Fatal("create lifecycle race lease:", err)
			}
			drainTx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin held lifecycle drain:", err)
			}
			if _, err := store.ApplyModuleLifecycleChangeTx(ctx, drainTx, tenant, "drain-operation", drainPlan.BaseRevision, drainPlan); err != nil {
				_ = drainTx.Rollback()
				t.Fatal("apply held lifecycle drain:", err)
			}
			admissionResult := make(chan error, 1)
			go func() { admissionResult <- store.AdmitCapabilityAttempt(ctx, lease, time.Now().UTC()) }()
			select {
			case admissionErr := <-admissionResult:
				_ = drainTx.Rollback()
				t.Fatalf("capability admission crossed an uncommitted drain fence: %v", admissionErr)
			case <-time.After(50 * time.Millisecond):
			}
			if err := drainTx.Commit(); err != nil {
				t.Fatal("commit lifecycle drain:", err)
			}
			select {
			case admissionErr := <-admissionResult:
				var denial *contract.Error
				if !errors.As(admissionErr, &denial) || denial.Type != contract.CodePermissionDenied {
					t.Fatalf("post-drain capability admission error = %v, want permission denied", admissionErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("capability admission did not resume after drain commit")
			}
			storedLease, found, err := store.LoadCapabilityLease(ctx, tenant, lease.ID)
			if err != nil || !found || storedLease.AdmittedAttempts != 0 {
				t.Fatalf("admission crossed committed drain: lease=%+v found=%t err=%v", storedLease, found, err)
			}
			draining, err := store.Load(tenant)
			if err != nil {
				t.Fatal("reload draining module:", err)
			}
			installation := draining.Installations[0]
			if installation.State != string(contract.ModuleDraining) || installation.LastLifecycleOperationID != "drain-operation" || installation.StateChangedAt.IsZero() {
				t.Fatalf("drain state was not durably recorded: %+v", installation)
			}

			inspectors := []contract.OpenWorkInspector{lifecycleIntegrationInspector{}}
			disableRequest := contract.PlanModuleChange{ModuleID: module.ID, TargetState: contract.ModuleDisabled, ExpectedRevision: draining.Revision}
			disablePlan, err := configstore.PlanModuleLifecycleChange(ctx, draining, disableRequest, inspectors...)
			if err != nil || len(disablePlan.OpenWorkBlockers) != 0 {
				t.Fatalf("plan module disable = %+v, %v", disablePlan, err)
			}
			applyLifecyclePlan(t, ctx, database, store, tenant, "disable-operation", disablePlan, inspectors...)
			disabled, err := store.Load(tenant)
			if err != nil {
				t.Fatal("reload disabled module:", err)
			}
			installation = disabled.Installations[0]
			if installation.State != string(contract.ModuleDisabled) || installation.LastLifecycleOperationID != "disable-operation" || installation.Revision != 3 {
				t.Fatalf("disabled state was not durably recorded: %+v", installation)
			}
		})
	}
}

func applyLifecyclePlan(t *testing.T, ctx context.Context, database *sql.DB, store *configstore.ModuleStore, tenant contract.TenantID, operation contract.OperationID, plan contract.ModuleChangePlan, inspectors ...contract.OpenWorkInspector) {
	t.Helper()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin lifecycle apply:", err)
	}
	defer tx.Rollback()
	if _, err := store.ApplyModuleLifecycleChangeTx(ctx, tx, tenant, operation, plan.BaseRevision, plan, inspectors...); err != nil {
		t.Fatal("apply lifecycle plan:", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit lifecycle plan:", err)
	}
}
