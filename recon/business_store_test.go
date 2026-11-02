package recon

import (
	"context"
	"encoding/json"
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

type allowBusinessRunRead struct{ err error }

func (a allowBusinessRunRead) CanReadBusinessRun(context.Context, contract.TenantID, string, string) error {
	return a.err
}

type allowBusinessFindingSummary struct{ err error }

func (a allowBusinessFindingSummary) CanInspectBusinessFindings(context.Context, contract.BusinessScope, string) error {
	return a.err
}

type allowBusinessRepair struct{ err error }

func (a allowBusinessRepair) CanRepairBusinessFinding(context.Context, contract.TenantID, contract.BusinessScope, string) error {
	return a.err
}

type countedBusinessRepairer struct{ calls int }

func (r *countedBusinessRepairer) RepairBusinessObligation(context.Context, BusinessObligation, contract.IdempotencyKey) (string, error) {
	r.calls++
	return "owner-evidence", nil
}

type uncertainBusinessRepairer struct {
	attempts    map[contract.IdempotencyKey]int
	effects     map[contract.IdempotencyKey]bool
	effectCount int
}

func (r *uncertainBusinessRepairer) RepairBusinessObligation(_ context.Context, _ BusinessObligation, key contract.IdempotencyKey) (string, error) {
	if r.attempts == nil {
		r.attempts = map[contract.IdempotencyKey]int{}
		r.effects = map[contract.IdempotencyKey]bool{}
	}
	r.attempts[key]++
	if !r.effects[key] {
		r.effects[key] = true
		r.effectCount++
	}
	if r.attempts[key] == 1 {
		return "", errors.New("owner applied command but response was lost")
	}
	return "owner-confirmed-effect", nil
}

func TestBusinessRunStorePersistsRunAndFindingsAtomically(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	run := BusinessRun{TenantID: "tenant-a", OwnerModuleID: "sales.orders", ID: "run-1", RuleID: "sales.invoice", Version: 1, Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, Cutoff: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Complete: true, Findings: []BusinessFinding{{ID: "bf_1", Key: "sale:1", Kind: FindingMissingEffect}}}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_business_recon_run (tenant_id, run_id, owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "run-1", "sales.orders", "sales.invoice", uint32(1), sqlmock.AnyArg(), run.Cutoff.Format(time.RFC3339Nano), int64(1), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_business_recon_finding (tenant_id, run_id, finding_id, finding_json, state, evidence) VALUES (?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "run-1", "bf_1", sqlmock.AnyArg(), "open", "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.Save(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunStoreLoadsDurableCursorAndWatermarks(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-2").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("sales.orders", "sales.invoice", 3, `{"tenant_id":"tenant-a","kind":"tenant"}`, "2026-11-02T00:00:00Z", 0, `{"source":"cursor-s","source_rows":[{"key":"sale:1","source_ref":"sale:1","entity_id":"entity-a","amount":"5.00","currency":"KES","occurred_at":"2026-11-02T00:00:00Z"}],"source_complete":true,"watermarks":{"source_revision":"source-r","source_snapshot":"source-snap"}}`, `{"source_revision":"source-r","target_revision":"target-r"}`, "2026-11-02T00:00:00Z"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-2").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}))
	run, err := store.Load(context.Background(), "tenant-a", "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if run.OwnerModuleID != "sales.orders" || run.Cursors.Source != "cursor-s" || len(run.Cursors.SourceRows) != 1 || !run.Cursors.SourceComplete || run.Cursors.Watermarks.SourceSnapshot != "source-snap" || run.Watermarks.SourceRevision != "source-r" || run.Watermarks.TargetRevision != "target-r" || run.Complete {
		t.Fatalf("durable run progress = %+v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunStoreInspectOpenWorkIsOwnerScoped(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_business_recon_finding f JOIN _kora_business_recon_run r ON r.tenant_id=f.tenant_id AND r.run_id=f.run_id WHERE f.tenant_id=? AND r.owner_module_id=? AND f.state IN (?,?)`)).WithArgs("tenant-a", "sales.orders", string(BusinessFindingOpen), string(BusinessFindingInvestigating)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT f.finding_id FROM _kora_business_recon_finding f JOIN _kora_business_recon_run r ON r.tenant_id=f.tenant_id AND r.run_id=f.run_id WHERE f.tenant_id=? AND r.owner_module_id=? AND f.state IN (?,?) ORDER BY f.finding_id LIMIT ?`)).WithArgs("tenant-a", "sales.orders", string(BusinessFindingOpen), string(BusinessFindingInvestigating), 2).WillReturnRows(sqlmock.NewRows([]string{"finding_id"}).AddRow("bf-a").AddRow("bf-b"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM _kora_business_recon_run WHERE tenant_id=? AND owner_module_id=? AND complete=?`)).WithArgs("tenant-a", "sales.orders", int64(0)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT run_id FROM _kora_business_recon_run WHERE tenant_id=? AND owner_module_id=? AND complete=? ORDER BY created_at,run_id LIMIT ?`)).WithArgs("tenant-a", "sales.orders", int64(0), 2).WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow("run-incomplete"))
	blockers, err := store.InspectOpenWork(context.Background(), "tenant-a", "sales.orders", 2)
	if err != nil || len(blockers) != 2 || blockers[0].Code != "domain_obligation_open" || blockers[0].Count != 2 || len(blockers[0].Examples) != 2 || blockers[1].Code != "domain_reconciliation_incomplete" || blockers[1].Count != 1 {
		t.Fatalf("owner-scoped obligation blockers = %+v, %v", blockers, err)
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
	run := BusinessRun{TenantID: "tenant-a", OwnerModuleID: "sales.orders", ID: "run-1", RuleID: "sales.invoice", Version: 1, Scope: contract.BusinessScope{TenantID: "tenant-b", Kind: contract.ScopeKindTenant}, Cutoff: time.Now().UTC()}
	if err := store.Save(context.Background(), run); err == nil {
		t.Fatal("cross-tenant business run accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunStoreAuthorizedReadDenialDoesNotQueryPersistence(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	if _, err := store.LoadAuthorized(context.Background(), "tenant-a", "run-1", "reader-a", allowBusinessRunRead{err: errors.New("denied")}); err == nil {
		t.Fatal("unauthorized business run read accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunStoreCountsOnlyUnresolvedTenantBusinessFindings(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM _kora_business_recon_finding WHERE tenant_id = ? AND state IN (?, ?)")).WithArgs("tenant-a", "open", "investigating").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	count, err := store.CountUnresolvedFindings(context.Background(), "tenant-a")
	if err != nil || count != 3 {
		t.Fatalf("unresolved business finding count = %d, %v", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunStoreCountsFindingsInsideExactBusinessScope(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	scopeJSON, _ := json.Marshal(scope)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM _kora_business_recon_finding f JOIN _kora_business_recon_run r ON r.tenant_id=f.tenant_id AND r.run_id=f.run_id WHERE f.tenant_id = ? AND r.scope_json = ? AND f.state IN (?, ?)")).WithArgs("tenant-a", string(scopeJSON), "open", "investigating").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	count, err := store.CountUnresolvedFindingsInScope(context.Background(), scope)
	if err != nil || count != 2 {
		t.Fatalf("scope-filtered unresolved business finding count=%d err=%v", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessReconciliationServiceAuthorizesFindingSummary(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := BusinessReconciliationService{Store: &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	if _, err := service.CountUnresolvedFindings(context.Background(), scope, "reader-a", allowBusinessFindingSummary{err: errors.New("denied")}); err == nil {
		t.Fatal("unauthorized finding summary accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessRunStoreCompletedRepairRetryDoesNotCallOwnerAgain(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	finding := BusinessFinding{ID: "bf_repair", Key: "sale:1", Kind: FindingMissingEffect, Expected: BusinessObligation{Key: "sale:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "4.00", Currency: "KES"}}
	findingJSON, err := json.Marshal(finding)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("sales.orders", "sales.invoice", 1, `{"tenant_id":"tenant-a","kind":"tenant"}`, "2026-11-02T00:00:00Z", 1, `{}`, `{}`, "2026-11-02T00:00:00Z"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}).AddRow(string(findingJSON), "resolved", "owner-evidence"))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? AND finding_id = ?")).WithArgs("tenant-a", "run-1", "bf_repair").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow("resolved"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key, state FROM _kora_business_recon_repair WHERE tenant_id = ? AND run_id = ? AND finding_id = ?")).WithArgs("tenant-a", "run-1", "bf_repair").WillReturnRows(sqlmock.NewRows([]string{"idempotency_key", "state"}).AddRow(string(businessRepairIdempotencyKey("tenant-a", "run-1", "bf_repair")), "completed"))
	mock.ExpectRollback()
	owner := &countedBusinessRepairer{}
	err = store.Repair(context.Background(), "tenant-a", "run-1", "bf_repair", "operator-a", allowBusinessRepair{}, owner)
	if err != nil || owner.calls != 0 {
		t.Fatalf("completed repair retry = err %v, owner calls %d", err, owner.calls)
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
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("sales.orders", "sales.invoice", 1, `{"tenant_id":"tenant-a","kind":"tenant"}`, "2026-11-02T00:00:00Z", 1, `{}`, `{}`, "2026-11-02T00:00:00Z"))
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
	mock2.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-1").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("sales.orders", "sales.invoice", 1, `{"tenant_id":"tenant-a","kind":"tenant"}`, "2026-11-02T00:00:00Z", 1, `{}`, `{}`, "2026-11-02T00:00:00Z"))
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
