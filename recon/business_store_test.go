package recon

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

type allowBusinessWaiver struct{ err error }

func (a allowBusinessWaiver) CanWaiveBusinessFinding(context.Context, contract.TenantID, contract.BusinessScope, string) error {
	return a.err
}

func TestBusinessRunStorePersistsRunAndFindingsAtomically(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	run := BusinessRun{TenantID: "tenant-a", ID: "run-1", RuleID: "sales.invoice", Version: 1, Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, Cutoff: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Complete: true, Findings: []BusinessFinding{{ID: "bf_1", Key: "sale:1", Kind: FindingMissingEffect}}}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_business_recon_run (tenant_id, run_id, rule_id, rule_version, scope_json, cutoff_at, complete, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "run-1", "sales.invoice", uint32(1), sqlmock.AnyArg(), run.Cutoff.Format(time.RFC3339Nano), int64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_business_recon_finding (tenant_id, run_id, finding_id, finding_json, state, evidence) VALUES (?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "run-1", "bf_1", sqlmock.AnyArg(), "open", "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunStoreRejectsForgedScopeAndFindingWithoutTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	run := BusinessRun{TenantID: "tenant-a", ID: "run-1", RuleID: "sales.invoice", Version: 1, Scope: contract.BusinessScope{TenantID: "tenant-b", Kind: contract.ScopeKindTenant}, Cutoff: time.Now().UTC()}
	if err := store.Save(context.Background(), run); err == nil {
		t.Fatal("cross-tenant business run accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunWaiverRequiresAuthorizationAndAuditsAtomically(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT rule_id, rule_version, scope_json, cutoff_at, complete, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "created_at"}).AddRow("sales.invoice", 1, `{"tenant_id":"tenant-a","kind":"tenant"}`, "2026-11-02T00:00:00Z", 1, "2026-11-02T00:00:00Z"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}))
	if err := store.Waive(context.Background(), "tenant-a", "run-1", "bf_1", "operator-a", "source owner confirms non-applicability", allowBusinessWaiver{err: errors.New("denied")}); err == nil {
		t.Fatal("unauthorized waiver accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	database2, mock2, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database2.Close()
	store.DB = database2
	mock2.ExpectQuery(regexp.QuoteMeta("SELECT rule_id, rule_version, scope_json, cutoff_at, complete, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "created_at"}).AddRow("sales.invoice", 1, `{"tenant_id":"tenant-a","kind":"tenant"}`, "2026-11-02T00:00:00Z", 1, "2026-11-02T00:00:00Z"))
	mock2.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}))
	mock2.ExpectBegin()
	mock2.ExpectQuery(regexp.QuoteMeta("SELECT state FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? AND finding_id = ?")).WithArgs("tenant-a", "run-1", "bf_1").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow("open"))
	mock2.ExpectExec(regexp.QuoteMeta("UPDATE _kora_business_recon_finding SET state = ?, evidence = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND state = ?")).WithArgs("waived", "source owner confirms non-applicability", "tenant-a", "run-1", "bf_1", "open").WillReturnResult(sqlmock.NewResult(0, 1))
	mock2.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_business_recon_audit (tenant_id, run_id, finding_id, actor_id, action, reason, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "run-1", "bf_1", "operator-a", "waive", "source owner confirms non-applicability", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock2.ExpectCommit()
	if err := store.Waive(context.Background(), "tenant-a", "run-1", "bf_1", "operator-a", "source owner confirms non-applicability", allowBusinessWaiver{}); err != nil {
		t.Fatal(err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRepairIdempotencyIdentityIsStableAndFindingScoped(t *testing.T) {
	a := businessRepairIdempotencyKey("tenant-a", "run-1", "finding-1")
	b := businessRepairIdempotencyKey("tenant-a", "run-1", "finding-1")
	c := businessRepairIdempotencyKey("tenant-a", "run-1", "finding-2")
	if a == "" || a != b || a == c {
		t.Fatalf("repair idempotency identity: %q %q %q", a, b, c)
	}
}
