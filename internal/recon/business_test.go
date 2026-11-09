package recon

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

func TestCompareBusinessObligationsFindingsAreStableAndScoped(t *testing.T) {
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	input := BusinessReconciliationInput{Cutoff: cutoff, GracePeriod: time.Hour, MaxRows: 20, SourceReady: true, TargetReady: true,
		Sources: []BusinessObligation{
			{Key: "invoice:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "12.50", Currency: "KES", OccurredAt: cutoff.Add(-2 * time.Hour)},
			{Key: "invoice:2", SourceRef: "sale:2", EntityID: "entity-a", Amount: "4.00", Currency: "KES", OccurredAt: cutoff.Add(-10 * time.Minute)},
			{Key: "invoice:3", SourceRef: "sale:3", EntityID: "entity-a", Amount: "8.00", Currency: "KES", OccurredAt: cutoff.Add(-2 * time.Hour)},
			{Key: "invoice:4", SourceRef: "sale:4", EntityID: "entity-a", Amount: "5.00", Currency: "KES", OccurredAt: cutoff.Add(-2 * time.Hour)},
		}, Targets: []BusinessEffect{
			{Key: "invoice:3", TargetRef: "invoice:b", EntityID: "entity-b", Amount: "8.00", Currency: "KES", OccurredAt: cutoff.Add(-time.Hour)},
			{Key: "invoice:3", TargetRef: "invoice:c", EntityID: "entity-a", Amount: "8.00", Currency: "KES", OccurredAt: cutoff.Add(-time.Hour)},
			{Key: "invoice:4", TargetRef: "invoice:d", EntityID: "entity-b", Amount: "5.00", Currency: "KES", OccurredAt: cutoff.Add(-time.Hour)},
		}}
	first, err := CompareBusinessObligations(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompareBusinessObligations(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Findings) != 4 || len(second.Findings) != len(first.Findings) {
		t.Fatalf("findings = %+v", first.Findings)
	}
	byKind := map[BusinessFindingKind]BusinessFinding{}
	for _, finding := range first.Findings {
		byKind[finding.Kind] = finding
	}
	if byKind[FindingMissingEffect].Key != "invoice:1" || byKind[FindingPendingGrace].Key != "invoice:2" || byKind[FindingDuplicateEffect].Key != "invoice:3" {
		t.Fatalf("unexpected finding kinds: %+v", first.Findings)
	}
	if byKind[FindingScopeMismatch].Key != "invoice:4" {
		t.Fatalf("wrong entity was not independently found: %+v", first.Findings)
	}
	for i := range first.Findings {
		if first.Findings[i].ID != second.Findings[i].ID {
			t.Fatal("finding identities changed across retry")
		}
	}
}

func TestCompareBusinessObligationsMarksUnavailableIncompleteAndEnforcesBounds(t *testing.T) {
	base := BusinessReconciliationInput{Cutoff: time.Now().UTC(), MaxRows: 1, SourceReady: true, TargetReady: false}
	result, err := CompareBusinessObligations(base)
	if err != nil || result.Complete || len(result.Findings) != 0 {
		t.Fatalf("unavailable target = %+v, %v", result, err)
	}
	base.TargetReady = true
	base.Sources = []BusinessObligation{{Key: "a"}, {Key: "b"}}
	if _, err := CompareBusinessObligations(base); err == nil {
		t.Fatal("over-limit run accepted")
	}
}

func TestCompareBusinessObligationsComparesExactDecimalValuesAndCurrency(t *testing.T) {
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	base := BusinessReconciliationInput{Cutoff: cutoff, MaxRows: 10, SourceReady: true, TargetReady: true,
		Sources: []BusinessObligation{{Key: "source:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "12.500", Currency: "KES", OccurredAt: cutoff.Add(-time.Hour)}},
		Targets: []BusinessEffect{{Key: "source:1", TargetRef: "invoice:1", EntityID: "entity-a", Amount: "12.50", Currency: "KES", OccurredAt: cutoff.Add(-time.Minute)}}}
	result, err := CompareBusinessObligations(base)
	if err != nil || len(result.Findings) != 0 {
		t.Fatalf("equivalent decimal scales differ: %+v, %v", result, err)
	}
	base.Targets[0].Currency = "USD"
	result, err = CompareBusinessObligations(base)
	if err != nil || len(result.Findings) != 1 || result.Findings[0].Kind != FindingAmountMismatch {
		t.Fatalf("currency mismatch not found: %+v, %v", result, err)
	}
}

func TestCompareBusinessObligationsDetectsLifecycleMismatch(t *testing.T) {
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	input := BusinessReconciliationInput{Cutoff: cutoff, MaxRows: 10, SourceReady: true, TargetReady: true,
		Sources: []BusinessObligation{{Key: "sale:1", EntityID: "entity-a", Amount: "12.50", Currency: "KES", Lifecycle: "posted", OccurredAt: cutoff.Add(-time.Hour)}},
		Targets: []BusinessEffect{{Key: "sale:1", TargetRef: "invoice:1", EntityID: "entity-a", Amount: "12.50", Currency: "KES", Lifecycle: "draft", OccurredAt: cutoff.Add(-time.Minute)}}}
	result, err := CompareBusinessObligations(input)
	if err != nil || len(result.Findings) != 1 || result.Findings[0].Kind != FindingLifecycleMismatch {
		t.Fatalf("lifecycle mismatch not found: %+v, %v", result, err)
	}
}

func TestExecuteBusinessReconciliationPagesOwnerQueriesAndPinsWatermarks(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	rule := BusinessReconciliationRule{OwnerModuleID: "sales.orders", ID: "sales-ar", Version: 2, Scope: scope,
		SourceQuery: BusinessQueryRef{OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{}`), PageLimit: 10},
		TargetQuery: BusinessQueryRef{OwnerModuleID: "ar.receivables", Name: "invoices", Input: json.RawMessage(`{}`), PageLimit: 10},
		GracePeriod: time.Hour, MaxRows: 10}
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	calls := []BusinessQueryRequest{}
	read := func(_ context.Context, request BusinessQueryRequest) (contract.QueryResult, error) {
		calls = append(calls, request)
		if request.Scope != scope || request.Limit == 0 {
			t.Fatalf("invalid owner query request: %+v", request)
		}
		var data string
		next := ""
		if request.Name == "billable" {
			if request.Cursor == "" {
				data, next = `[{"key":"sale:1","source_ref":"sale:1","entity_id":"entity-a","amount":"12.00","currency":"KES","occurred_at":"2026-11-02T08:00:00Z"}]`, "source-page-2"
			} else {
				data = `[{"key":"sale:2","source_ref":"sale:2","entity_id":"entity-a","amount":"2.00","currency":"KES","occurred_at":"2026-11-02T08:00:00Z"}]`
			}
		} else {
			data = `[{"key":"sale:1","target_ref":"invoice:1","entity_id":"entity-a","amount":"12.00","currency":"KES","occurred_at":"2026-11-02T09:00:00Z"}]`
		}
		return contract.QueryResult{Data: json.RawMessage(data), NextCursor: next, SnapshotToken: "snapshot-1", SourceRevision: "revision-1", AsOf: cutoff, Freshness: contract.QueryFresh}, nil
	}
	result, cursors, watermarks, err := ExecuteBusinessReconciliation(context.Background(), rule, cutoff, BusinessRunCursor{}, read)
	if err != nil || !result.Complete || len(result.Findings) != 1 || result.Findings[0].Kind != FindingMissingEffect {
		t.Fatalf("business run = %+v, cursors=%+v watermarks=%+v, err=%v", result, cursors, watermarks, err)
	}
	if cursors.Source != "" || cursors.Target != "" || watermarks.SourceRevision != "revision-1" || watermarks.TargetSnapshot != "snapshot-1" || len(calls) != 3 {
		t.Fatalf("query pagination metadata = calls:%+v cursors:%+v watermarks:%+v", calls, cursors, watermarks)
	}
}

func TestExecuteBusinessReconciliationUnavailableOwnerReturnsIncomplete(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	rule := BusinessReconciliationRule{OwnerModuleID: "sales.orders", ID: "sales-ar", Version: 1, Scope: scope,
		SourceQuery: BusinessQueryRef{OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{}`), PageLimit: 10},
		TargetQuery: BusinessQueryRef{OwnerModuleID: "ar.receivables", Name: "invoices", Input: json.RawMessage(`{}`), PageLimit: 10}, MaxRows: 10}
	for _, unavailable := range []string{"billable", "invoices"} {
		t.Run(unavailable, func(t *testing.T) {
			read := func(_ context.Context, request BusinessQueryRequest) (contract.QueryResult, error) {
				if request.Name == unavailable {
					return contract.QueryResult{}, context.DeadlineExceeded
				}
				return contract.QueryResult{Data: json.RawMessage(`[]`), SnapshotToken: "s", SourceRevision: "r", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}, nil
			}
			result, _, _, err := ExecuteBusinessReconciliation(context.Background(), rule, time.Now().UTC(), BusinessRunCursor{}, read)
			if err != nil || result.Complete || len(result.Findings) != 0 {
				t.Fatalf("unavailable %s owner must make run incomplete: %+v, %v", unavailable, result, err)
			}
		})
	}
}

func TestExecuteBusinessReconciliationResumesWithPersistedEarlierPages(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	rule := BusinessReconciliationRule{OwnerModuleID: "sales.orders", ID: "sales-ar", Version: 1, Scope: scope,
		SourceQuery: BusinessQueryRef{OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{}`), PageLimit: 10},
		TargetQuery: BusinessQueryRef{OwnerModuleID: "ar.receivables", Name: "invoices", Input: json.RawMessage(`{}`), PageLimit: 10}, MaxRows: 10}
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	prior := BusinessObligation{Key: "sale:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "10.00", Currency: "KES", OccurredAt: cutoff.Add(-time.Hour)}
	marks := BusinessRunWatermarks{SourceRevision: "rev-1", SourceSnapshot: "snap-1", SourceAsOf: cutoff}
	reads := 0
	result, next, _, err := ExecuteBusinessReconciliation(context.Background(), rule, cutoff, BusinessRunCursor{Source: "source-page-2", SourceRows: []BusinessObligation{prior}, Watermarks: marks}, func(_ context.Context, request BusinessQueryRequest) (contract.QueryResult, error) {
		reads++
		data := `[]`
		if request.Name == "billable" {
			data = `[{"key":"sale:2","source_ref":"sale:2","entity_id":"entity-a","amount":"4.00","currency":"KES","occurred_at":"2026-11-02T10:00:00Z"}]`
		}
		return contract.QueryResult{Data: json.RawMessage(data), SnapshotToken: "snap-1", SourceRevision: "rev-1", AsOf: cutoff, Freshness: contract.QueryFresh}, nil
	})
	if err != nil || !result.Complete || reads != 2 || len(result.Findings) != 2 || next.Source != "" || len(next.SourceRows) != 2 {
		t.Fatalf("resumed comparison lost earlier-page evidence: result=%+v next=%+v reads=%d err=%v", result, next, reads, err)
	}
}

func TestExecuteBusinessReconciliationDoesNotRereadCompletedSideOnResume(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	rule := BusinessReconciliationRule{OwnerModuleID: "sales.orders", ID: "sales-ar", Version: 1, Scope: scope,
		SourceQuery: BusinessQueryRef{OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{}`), PageLimit: 10},
		TargetQuery: BusinessQueryRef{OwnerModuleID: "ar.receivables", Name: "invoices", Input: json.RawMessage(`{}`), PageLimit: 10}, MaxRows: 1}
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var sourceCalls, targetCalls int
	read := func(_ context.Context, request BusinessQueryRequest) (contract.QueryResult, error) {
		data, next := `[]`, ""
		if request.Name == "billable" {
			sourceCalls++
			data = `[{"key":"sale:1","source_ref":"sale:1","entity_id":"entity-a","amount":"10.00","currency":"KES","occurred_at":"2026-11-02T10:00:00Z"}]`
		} else {
			targetCalls++
			if request.Cursor == "" {
				data, next = `[{"key":"sale:1","target_ref":"invoice:1","entity_id":"entity-a","amount":"10.00","currency":"KES","occurred_at":"2026-11-02T10:30:00Z"}]`, "target-page-2"
			}
		}
		return contract.QueryResult{Data: json.RawMessage(data), NextCursor: next, SnapshotToken: "snap-1", SourceRevision: "rev-1", AsOf: cutoff, Freshness: contract.QueryFresh}, nil
	}
	_, cursor, _, err := ExecuteBusinessReconciliation(context.Background(), rule, cutoff, BusinessRunCursor{}, read)
	if err != nil || cursor.SourceComplete != true || cursor.TargetComplete || cursor.Target != "target-page-2" {
		t.Fatalf("first bounded pass did not persist side completion: cursor=%+v err=%v", cursor, err)
	}
	result, _, _, err := ExecuteBusinessReconciliation(context.Background(), rule, cutoff, cursor, read)
	if err != nil || !result.Complete || len(result.Findings) != 0 || sourceCalls != 1 || targetCalls != 2 {
		t.Fatalf("resume reread completed source or lost target progress: result=%+v sourceCalls=%d targetCalls=%d err=%v", result, sourceCalls, targetCalls, err)
	}
}

type businessPublishedPlanStub struct {
	requests []BusinessQueryRequest
	result   contract.QueryResult
}

func (s *businessPublishedPlanStub) ExecutePublishedQuery(_ context.Context, scope contract.BusinessScope, owner contract.ModuleID, name string, input json.RawMessage, cursor string, limit uint16) (contract.QueryResult, error) {
	s.requests = append(s.requests, BusinessQueryRequest{Scope: scope, OwnerModuleID: owner, Name: name, Input: input, Cursor: cursor, Limit: limit})
	return s.result, nil
}

func TestPublishedBusinessQueryReaderUsesExistingBindingPlanContract(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	plan := &businessPublishedPlanStub{result: contract.QueryResult{Data: json.RawMessage(`[]`), AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}}
	reader := PublishedBusinessQueryReader(plan)
	request := BusinessQueryRequest{Scope: scope, OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{"status":"posted"}`), Cursor: "cursor-1", Limit: 25}
	if _, err := reader(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(plan.requests) != 1 || plan.requests[0].Scope != scope || plan.requests[0].OwnerModuleID != request.OwnerModuleID || plan.requests[0].Name != request.Name || string(plan.requests[0].Input) != string(request.Input) || plan.requests[0].Cursor != request.Cursor || plan.requests[0].Limit != request.Limit {
		t.Fatalf("published binding plan request = %+v", plan.requests)
	}
}

type capturedBusinessRunRepository struct{ run BusinessRun }

func (r *capturedBusinessRunRepository) Save(_ context.Context, run BusinessRun) error {
	r.run = run
	return nil
}

type businessRunAuthorizer struct{ err error }

func (a businessRunAuthorizer) CanRunBusinessReconciliation(context.Context, contract.TenantID, contract.BusinessScope, string) error {
	return a.err
}

func TestBusinessReconciliationServicePersistsUnavailableRunAsIncomplete(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	rule := BusinessReconciliationRule{OwnerModuleID: "sales.orders", ID: "sales-ar", Version: 1, Scope: scope,
		SourceQuery: BusinessQueryRef{OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{}`), PageLimit: 10},
		TargetQuery: BusinessQueryRef{OwnerModuleID: "ar.receivables", Name: "invoices", Input: json.RawMessage(`{}`), PageLimit: 10}, MaxRows: 10}
	repository := &capturedBusinessRunRepository{}
	service := BusinessReconciliationService{Runs: repository, ReadPage: func(_ context.Context, request BusinessQueryRequest) (contract.QueryResult, error) {
		if request.Name == "invoices" {
			return contract.QueryResult{}, context.DeadlineExceeded
		}
		return contract.QueryResult{Data: json.RawMessage(`[]`), SnapshotToken: "source-snapshot", SourceRevision: "source-revision", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}, nil
	}}
	run, err := service.Run(context.Background(), "operator-a", "run-unavailable", rule, time.Now().UTC(), businessRunAuthorizer{})
	if err != nil || run.Complete || repository.run.ID != "run-unavailable" || repository.run.Complete || repository.run.Watermarks.SourceRevision != "source-revision" {
		t.Fatalf("persist incomplete run: returned=%+v stored=%+v err=%v", run, repository.run, err)
	}
}

func TestBusinessReconciliationServiceAuthorizesBeforeOwnerQuery(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	rule := BusinessReconciliationRule{OwnerModuleID: "sales.orders", ID: "sales-ar", Version: 1, Scope: scope,
		SourceQuery: BusinessQueryRef{OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{}`), PageLimit: 10},
		TargetQuery: BusinessQueryRef{OwnerModuleID: "ar.receivables", Name: "invoices", Input: json.RawMessage(`{}`), PageLimit: 10}, MaxRows: 10}
	repository := &capturedBusinessRunRepository{}
	reads := 0
	service := BusinessReconciliationService{Runs: repository, ReadPage: func(context.Context, BusinessQueryRequest) (contract.QueryResult, error) {
		reads++
		return contract.QueryResult{}, nil
	}}
	if _, err := service.Run(context.Background(), "operator-a", "run-denied", rule, time.Now().UTC(), businessRunAuthorizer{err: context.Canceled}); err == nil {
		t.Fatal("unauthorized reconciliation run accepted")
	}
	if reads != 0 || repository.run.ID != "" {
		t.Fatalf("authorization denial reached owner query or persisted a run: reads=%d run=%+v", reads, repository.run)
	}
}

func TestBusinessReconciliationContinueLoadsAuthorizedPersistedCursor(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	cutoff := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	prior := BusinessObligation{Key: "sale:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "10.00", Currency: "KES", OccurredAt: cutoff.Add(-time.Hour)}
	priorRows, _ := json.Marshal([]BusinessObligation{prior})
	cursors := `{"source":"source-page-2","source_rows":` + string(priorRows) + `,"watermarks":{"source_revision":"rev-1","source_snapshot":"snap-1","source_as_of":"2026-11-02T12:00:00Z"}}`
	store := &BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-prior").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("sales.orders", "sales-ar", 1, `{"tenant_id":"tenant-a","kind":"tenant"}`, cutoff.Format(time.RFC3339Nano), 0, cursors, `{"source_revision":"rev-1","source_snapshot":"snap-1","source_as_of":"2026-11-02T12:00:00Z"}`, cutoff.Format(time.RFC3339Nano)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-prior").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}))
	repository := &capturedBusinessRunRepository{}
	service := BusinessReconciliationService{Runs: repository, Store: store, ReadPage: func(_ context.Context, request BusinessQueryRequest) (contract.QueryResult, error) {
		data := `[]`
		if request.Name == "billable" {
			data = `[{"key":"sale:2","source_ref":"sale:2","entity_id":"entity-a","amount":"4.00","currency":"KES","occurred_at":"2026-11-02T10:00:00Z"}]`
		}
		return contract.QueryResult{Data: json.RawMessage(data), SnapshotToken: "snap-1", SourceRevision: "rev-1", AsOf: cutoff, Freshness: contract.QueryFresh}, nil
	}}
	rule := BusinessReconciliationRule{OwnerModuleID: "sales.orders", ID: "sales-ar", Version: 1, Scope: scope, MaxRows: 10,
		SourceQuery: BusinessQueryRef{OwnerModuleID: "sales.orders", Name: "billable", Input: json.RawMessage(`{}`), PageLimit: 10},
		TargetQuery: BusinessQueryRef{OwnerModuleID: "ar.receivables", Name: "invoices", Input: json.RawMessage(`{}`), PageLimit: 10}}
	run, err := service.Continue(context.Background(), "operator-a", "run-next", "run-prior", rule, businessRunAuthorizer{}, allowBusinessRunRead{})
	if err != nil || !run.Complete || len(run.Findings) != 2 || len(repository.run.Findings) != 2 {
		t.Fatalf("authorized durable continuation lost earlier page: returned=%+v stored=%+v err=%v", run, repository.run, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
