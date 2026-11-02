//go:build integration

package recon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestBusinessRunPersistenceAcrossDisposableDialects(t *testing.T) {
	for _, tc := range []struct {
		name, driver, env string
		dialect           db.Dialect
	}{
		{"mysql", "mysql", "KORA_RECON_DISPOSABLE_MYSQL_DSN", &db.MySQLDialect{}},
		{"postgres", "postgres", "KORA_RECON_DISPOSABLE_POSTGRES_DSN", &db.PostgresDialect{}},
		{"libsql", "libsql", "KORA_RECON_DISPOSABLE_LIBSQL_DSN", &db.LibSQLDialect{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip("set " + tc.env + " to a fresh disposable database")
			}
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("open disposable database:", err)
			}
			defer database.Close()
			database.SetMaxOpenConns(2)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := database.PingContext(ctx); err != nil {
				t.Fatal("ping disposable database:", err)
			}
			store := &BusinessRunStore{DB: database, Dialect: tc.dialect}
			if err := store.EnsureSchema(ctx); err != nil {
				t.Fatal("ensure reconciliation schema:", err)
			}
			tenant := contract.TenantID(fmt.Sprintf("recon-test-%d", time.Now().UTC().UnixNano()))
			runID := "run-" + string(tenant)
			scope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
			run := BusinessRun{TenantID: tenant, OwnerModuleID: "sales.orders", ID: runID, RuleID: "sales-ar", Version: 1, Scope: scope, Cutoff: time.Now().UTC(), Complete: false, Cursors: BusinessRunCursor{Source: "cursor-source"}, Watermarks: BusinessRunWatermarks{SourceRevision: "revision-source"}, Findings: []BusinessFinding{{ID: "bf_live", Key: "sale:1", Kind: FindingMissingEffect}}}
			if err := store.Save(ctx, run); err != nil {
				t.Fatal("save business run:", err)
			}
			loaded, err := store.Load(ctx, tenant, runID)
			if err != nil {
				t.Fatal("load business run:", err)
			}
			if loaded.Cursors.Source != run.Cursors.Source || loaded.Watermarks.SourceRevision != run.Watermarks.SourceRevision || len(loaded.Findings) != 1 || loaded.Findings[0].ID != "bf_live" {
				t.Fatalf("loaded business evidence = %+v", loaded)
			}
			blockers, err := store.InspectOpenWork(ctx, tenant, "sales.orders", 5)
			if err != nil || len(blockers) != 2 || blockers[0].Code != "domain_obligation_open" || blockers[0].Count != 1 || blockers[1].Code != "domain_reconciliation_incomplete" || blockers[1].Count != 1 {
				t.Fatalf("owner-scoped lifecycle blockers = %+v, %v", blockers, err)
			}
			otherOwner, err := store.InspectOpenWork(ctx, tenant, "billing.ar", 5)
			if err != nil || len(otherOwner) != 0 {
				t.Fatalf("reconciliation findings leaked across module owners: %+v, %v", otherOwner, err)
			}
			if err := store.Waive(ctx, tenant, runID, "bf_live", "operator-a", "source owner confirms non-applicability", allowBusinessWaiver{}); err != nil {
				t.Fatal("authorized audited waiver:", err)
			}
			repairFinding := BusinessFinding{ID: "bf_repair_live", Key: "sale:repair", Kind: FindingMissingEffect, Expected: BusinessObligation{Key: "sale:repair", SourceRef: "sale:repair", EntityID: "entity-a", Amount: "3.00", Currency: "KES"}}
			repairRun := BusinessRun{TenantID: tenant, OwnerModuleID: "sales.orders", ID: runID + "-repair", RuleID: "sales-ar", Version: 1, Scope: scope, Cutoff: time.Now().UTC(), Complete: true, Findings: []BusinessFinding{repairFinding}}
			if err := store.Save(ctx, repairRun); err != nil {
				t.Fatal("save repair finding:", err)
			}
			if count, err := store.CountUnresolvedFindings(ctx, tenant); err != nil || count != 1 {
				t.Fatalf("unresolved obligation summary count=%d err=%v", count, err)
			}
			owner := &countedBusinessRepairer{}
			if err := store.Repair(ctx, tenant, repairRun.ID, repairFinding.ID, "operator-a", allowBusinessRepair{}, owner); err != nil {
				t.Fatal("owner-command repair:", err)
			}
			if err := store.Repair(ctx, tenant, repairRun.ID, repairFinding.ID, "operator-a", allowBusinessRepair{}, owner); err != nil || owner.calls != 1 {
				t.Fatalf("idempotent repair retry calls=%d err=%v", owner.calls, err)
			}
			restarted := &BusinessRunStore{DB: database, Dialect: tc.dialect}
			waived, err := restarted.Load(ctx, tenant, runID)
			if err != nil || len(waived.Findings) != 1 || waived.Findings[0].State != BusinessFindingWaived {
				t.Fatalf("reloaded waived finding = %+v, %v", waived.Findings, err)
			}
			repaired, err := restarted.Load(ctx, tenant, repairRun.ID)
			if err != nil || len(repaired.Findings) != 1 || repaired.Findings[0].State != BusinessFindingResolved || repaired.Findings[0].Evidence != "owner-evidence" {
				t.Fatalf("reloaded repaired finding = %+v, %v", repaired.Findings, err)
			}
			if count, err := restarted.CountUnresolvedFindings(ctx, tenant); err != nil || count != 0 {
				t.Fatalf("waived and repaired obligation summary count=%d err=%v", count, err)
			}
			uncertainFinding := BusinessFinding{ID: "bf_uncertain_live", Key: "sale:uncertain", Kind: FindingMissingEffect, Expected: BusinessObligation{Key: "sale:uncertain", SourceRef: "sale:uncertain", EntityID: "entity-a", Amount: "6.00", Currency: "KES"}}
			uncertainRun := BusinessRun{TenantID: tenant, OwnerModuleID: "sales.orders", ID: runID + "-uncertain", RuleID: "sales-ar", Version: 1, Scope: scope, Cutoff: time.Now().UTC(), Complete: true, Findings: []BusinessFinding{uncertainFinding}}
			if err := store.Save(ctx, uncertainRun); err != nil {
				t.Fatal("save uncertain repair finding:", err)
			}
			ownerWithLostResponse := &uncertainBusinessRepairer{}
			if err := store.Repair(ctx, tenant, uncertainRun.ID, uncertainFinding.ID, "operator-a", allowBusinessRepair{}, ownerWithLostResponse); err == nil {
				t.Fatal("lost owner response was treated as confirmed success")
			}
			pending, err := store.Load(ctx, tenant, uncertainRun.ID)
			if err != nil || len(pending.Findings) != 1 || pending.Findings[0].State != BusinessFindingInvestigating {
				t.Fatalf("uncertain owner result was not retained for reconciliation: %+v, %v", pending.Findings, err)
			}
			if err := restarted.Repair(ctx, tenant, uncertainRun.ID, uncertainFinding.ID, "operator-a", allowBusinessRepair{}, ownerWithLostResponse); err != nil {
				t.Fatal("restart retry with stable owner idempotency:", err)
			}
			if len(ownerWithLostResponse.attempts) != 1 || ownerWithLostResponse.effectCount != 1 {
				t.Fatalf("repair changed idempotency key or duplicated owner effect: attempts=%+v effects=%d", ownerWithLostResponse.attempts, ownerWithLostResponse.effectCount)
			}
			for _, attempts := range ownerWithLostResponse.attempts {
				if attempts != 2 {
					t.Fatalf("owner retry attempts=%d, want 2 with one deduplicated effect", attempts)
				}
			}
			if count, err := restarted.CountUnresolvedFindings(ctx, tenant); err != nil || count != 0 {
				t.Fatalf("resolved uncertain finding summary count=%d err=%v", count, err)
			}
			var auditCount int
			if err := database.QueryRowContext(ctx, db.Rebind(tc.dialect, `SELECT COUNT(*) FROM _kora_business_recon_audit WHERE tenant_id = ?`), string(tenant)).Scan(&auditCount); err != nil || auditCount != 5 {
				t.Fatalf("waiver and repair audit count=%d err=%v", auditCount, err)
			}
			if err := store.Save(ctx, run); err == nil {
				t.Fatal("reused immutable run ID")
			}
			loaded, err = store.Load(ctx, tenant, runID)
			if err != nil || loaded.Cursors.Source != run.Cursors.Source || len(loaded.Findings) != 1 {
				t.Fatalf("duplicate run altered persisted evidence: %+v, %v", loaded, err)
			}
			for _, table := range []string{"_kora_business_recon_audit", "_kora_business_recon_repair", "_kora_business_recon_finding", "_kora_business_recon_run"} {
				if _, err := database.ExecContext(ctx, db.Rebind(tc.dialect, "DELETE FROM "+table+" WHERE tenant_id = ?"), string(tenant)); err != nil {
					t.Fatalf("cleanup %s: %v", table, err)
				}
			}
		})
	}
}
