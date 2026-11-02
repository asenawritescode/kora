package recon

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

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
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_business_recon_finding (tenant_id, run_id, finding_id, finding_json) VALUES (?, ?, ?, ?)")).WithArgs("tenant-a", "run-1", "bf_1", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
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
