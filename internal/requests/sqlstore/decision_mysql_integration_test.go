//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/requests"
	"github.com/go-sql-driver/mysql"
)

// TestMySQLDecisionRace proves competing response transactions cannot create
// duplicate decisions or partially advance request state on real MySQL.
func TestMySQLDecisionRace(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open MySQL:", err)
	}
	if err := root.Ping(); err != nil {
		root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_request_race_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply disposable outbox schema: %v", err)
		}
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_auth_revision (
		id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL
	)`); err != nil {
		t.Fatal("create disposable authorization revision table:", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_auth_revision (id, policy_revision, membership_revision) VALUES (1, 1, 1)`); err != nil {
		t.Fatal("seed disposable authorization revision:", err)
	}
	statements, err := SchemaStatements(dialect)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply disposable request schema: %v", err)
		}
	}
	store, err := New(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	actor := contract.PrincipalID("reviewer-1")
	record := requests.RequestRecord{Tenant: "tenant-1", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-1",
		Title: "Concurrent review", Message: "Choose one response", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin request fixture:", err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatal("insert request fixture:", err)
	}
	assignment := requests.AssignmentRecord{ID: assignmentID, RequestID: requestID, Principal: actor,
		OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(actor)}, Stage: 1,
		Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
	if err := insertAssignment(ctx, tx, dialect, record.Tenant, assignment, now); err != nil {
		_ = tx.Rollback()
		t.Fatal("insert assignment fixture:", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit request and assignment fixtures:", err)
	}

	changes := make([]requests.DecisionCommit, 2)
	for i := range changes {
		change := decisionTestCommit("any")
		change.Request = record
		change.ExpectedRevision = record.Revision
		change.ExpectedPolicyRevision, change.ExpectedMembershipRevision = "1", "1"
		change.Decision.ID = contract.DecisionID(contract.NewID())
		change.Decision.RequestID, change.Decision.AssignmentID = requestID, assignmentID
		change.Decision.Actor, change.Decision.Tenant = actor, record.Tenant
		change.Decision.Response, change.Decision.Comment, change.Decision.At = contract.ResponseComplete, fmt.Sprintf("response-%d", i), now
		change.Identity.Tenant, change.Identity.Actor = record.Tenant, actor
		change.Identity.IdempotencyKey = contract.IdempotencyKey(fmt.Sprintf("race-response-%d", i))
		change.Receipt = contract.ResponseReceipt{RequestID: requestID, DecisionID: change.Decision.ID}
		change.Events = []contract.EventEnvelope{{ID: contract.NewEventID(), Type: "request.responded", Version: contract.CurrentVersion,
			Source: "kora.engine", Site: "tenant-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now,
			Actor: contract.ActorContext{PrincipalID: string(actor), PrincipalType: contract.PrincipalHuman, Site: "tenant-route", AuthenticatedAt: now},
			Data: contract.MustEncodeData(struct {
				RequestID contract.RequestID `json:"request_id"`
				Revision  contract.Revision  `json:"revision"`
			}{requestID, "2"})}}
		changes[i] = change
	}
	start := make(chan struct{})
	type result struct {
		receipt contract.ResponseReceipt
		change  requests.DecisionCommit
		err     error
	}
	results := make(chan result, len(changes))
	var wait sync.WaitGroup
	for _, change := range changes {
		wait.Add(1)
		go func(change requests.DecisionCommit) {
			defer wait.Done()
			<-start
			receipt, err := store.CommitDecision(ctx, change)
			results <- result{receipt: receipt, change: change, err: err}
		}(change)
	}
	close(start)
	wait.Wait()
	close(results)
	committed, conflicted := 0, 0
	var winner result
	for result := range results {
		if result.err == nil {
			committed++
			winner = result
			if result.receipt.Status != contract.RequestResolved {
				t.Errorf("winner receipt status = %q, want resolved", result.receipt.Status)
			}
		} else {
			var coded *contract.Error
			if errors.As(result.err, &coded) && coded.Type == contract.CodeConflict {
				conflicted++
			} else {
				t.Errorf("decision race returned unexpected error: %v", result.err)
			}
		}
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("decision race outcomes committed=%d conflicted=%d; want 1 and 1", committed, conflicted)
	}
	proveStaleRequestReconciliation(t, ctx, database, dialect, store, now)
	replayed, err := store.CommitDecision(ctx, winner.change)
	if err != nil {
		t.Fatal("retry winning idempotency key:", err)
	}
	if !reflect.DeepEqual(replayed, winner.receipt) {
		t.Fatalf("duplicate response receipt = %#v, original = %#v", replayed, winner.receipt)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`:                                     1,
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind IN ('responded','resolved')`: 2,
		`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND event_type = 'request.responded'`:                                  1,
		`SELECT COUNT(*) FROM _kora_request_idempotency WHERE tenant_id = ? AND operation = ?`:                                   1,
	} {
		var got int
		args := []any{"tenant-1", string(requestID)}
		if query == `SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND event_type = 'request.responded'` {
			args = []any{"tenant-route"}
		} else if query == `SELECT COUNT(*) FROM _kora_request_idempotency WHERE tenant_id = ? AND operation = ?` {
			args = []any{"tenant-1", "request.respond"}
		}
		if err := database.QueryRowContext(ctx, query, args...).Scan(&got); err != nil {
			t.Fatalf("count committed rows for %q: %v", query, err)
		}
		if got != want {
			t.Errorf("rows for %q = %d, want %d", query, got, want)
		}
	}
	proveDecisionCallerTransaction(t, ctx, database, dialect, store, now)
}

func proveStaleRequestReconciliation(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store, now time.Time) {
	t.Helper()
	id := contract.RequestID(contract.NewID())
	record := requests.RequestRecord{Tenant: "tenant-1", ID: id, Revision: "1", Kind: contract.RequestApproval,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-1",
		Subject: &contract.SubjectRef{Kind: "Invoice", ID: "invoice-stale-1", Revision: "1"}, SubjectDigest: strings.Repeat("a", 64),
		Title: "Subject edit", Message: "Check the latest invoice", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin stale request fixture:", err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatal("insert stale request fixture:", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit stale request fixture:", err)
	}
	observed := contract.SubjectRef{Kind: "Invoice", ID: "invoice-stale-1", Revision: "2"}
	if err := store.MarkStale(ctx, record.Tenant, id, "1", observed); err != nil {
		t.Fatal("mark changed subject stale:", err)
	}
	if err := store.MarkStale(ctx, record.Tenant, id, "1", observed); err != nil {
		t.Fatal("retry stale reconciliation:", err)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'stale'`:   1,
		`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type = 'request.stale'`: 1,
	} {
		var got int
		if err := database.QueryRowContext(ctx, query, string(record.Tenant), string(id)).Scan(&got); err != nil {
			t.Fatalf("count stale effects for %q: %v", query, err)
		}
		if got != want {
			t.Errorf("stale effects for %q = %d, want %d", query, got, want)
		}
	}
}

func proveDecisionCallerTransaction(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store, now time.Time) {
	t.Helper()
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_test_domain_effect (id VARCHAR(64) PRIMARY KEY)`); err != nil {
		t.Fatal("create disposable domain-effect table:", err)
	}
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	actor := contract.PrincipalID("reviewer-transaction")
	record := requests.RequestRecord{Tenant: "tenant-1", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-1", Title: "Atomic effect", Message: "Apply one effect", CreatedAt: now, UpdatedAt: now}
	fixture, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin caller-transaction fixture:", err)
	}
	if err := insertRequest(ctx, fixture, dialect, record); err != nil {
		_ = fixture.Rollback()
		t.Fatal("insert caller-transaction request fixture:", err)
	}
	assignment := requests.AssignmentRecord{ID: assignmentID, RequestID: requestID, Principal: actor,
		OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(actor)}, Stage: 1,
		Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
	if err := insertAssignment(ctx, fixture, dialect, record.Tenant, assignment, now); err != nil {
		_ = fixture.Rollback()
		t.Fatal("insert caller-transaction assignment fixture:", err)
	}
	if err := fixture.Commit(); err != nil {
		t.Fatal("commit caller-transaction request fixture:", err)
	}
	change := decisionTestCommit("any")
	change.Request, change.ExpectedRevision = record, record.Revision
	change.ExpectedPolicyRevision, change.ExpectedMembershipRevision = "1", "1"
	change.Decision.ID = contract.DecisionID(contract.NewID())
	change.Decision.RequestID, change.Decision.AssignmentID = requestID, assignmentID
	change.Decision.Tenant, change.Decision.Actor = record.Tenant, actor
	change.Decision.Response, change.Decision.At = contract.ResponseComplete, now
	change.Identity.Tenant, change.Identity.Actor = record.Tenant, actor
	change.Identity.IdempotencyKey = contract.IdempotencyKey("caller-tx-response-" + string(requestID))
	change.Receipt = contract.ResponseReceipt{RequestID: requestID, DecisionID: change.Decision.ID}
	change.Events = []contract.EventEnvelope{{ID: contract.NewEventID(), Type: "request.responded", Version: contract.CurrentVersion,
		Source: "kora.engine", Site: "tenant-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now,
		Actor: contract.ActorContext{PrincipalID: string(actor), PrincipalType: contract.PrincipalHuman, Site: "tenant-route", AuthenticatedAt: now},
		Data: contract.MustEncodeData(struct {
			RequestID contract.RequestID `json:"request_id"`
			Revision  contract.Revision  `json:"revision"`
		}{requestID, "2"})}}

	rollbackTx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin decision rollback transaction:", err)
	}
	if _, err := rollbackTx.ExecContext(ctx, `INSERT INTO _kora_test_domain_effect (id) VALUES (?)`, string(requestID)); err != nil {
		_ = rollbackTx.Rollback()
		t.Fatal("write disposable domain effect:", err)
	}
	if _, err := store.CommitDecisionInTx(ctx, rollbackTx, change); err != nil {
		_ = rollbackTx.Rollback()
		t.Fatal("write decision in caller transaction:", err)
	}
	if err := rollbackTx.Rollback(); err != nil {
		t.Fatal("roll back caller transaction:", err)
	}
	var effects, decisions int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_test_domain_effect WHERE id = ?`, string(requestID)).Scan(&effects); err != nil {
		t.Fatal("count rolled-back domain effects:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`, string(record.Tenant), string(requestID)).Scan(&decisions); err != nil {
		t.Fatal("count rolled-back decisions:", err)
	}
	if effects != 0 || decisions != 0 {
		t.Fatalf("caller rollback left domain effects=%d decisions=%d; want both zero", effects, decisions)
	}

	commitTx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin decision commit transaction:", err)
	}
	if _, err := commitTx.ExecContext(ctx, `INSERT INTO _kora_test_domain_effect (id) VALUES (?)`, string(requestID)); err != nil {
		_ = commitTx.Rollback()
		t.Fatal("write committed disposable domain effect:", err)
	}
	receipt, err := store.CommitDecisionInTx(ctx, commitTx, change)
	if err != nil {
		_ = commitTx.Rollback()
		t.Fatal("write committed decision in caller transaction:", err)
	}
	if err := commitTx.Commit(); err != nil {
		t.Fatal("commit caller transaction:", err)
	}
	if receipt.Status != contract.RequestResolved {
		t.Fatalf("transaction-bound response status = %q, want resolved", receipt.Status)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_test_domain_effect WHERE id = ?`, string(requestID)).Scan(&effects); err != nil {
		t.Fatal("count committed domain effects:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`, string(record.Tenant), string(requestID)).Scan(&decisions); err != nil {
		t.Fatal("count committed decisions:", err)
	}
	if effects != 1 || decisions != 1 {
		t.Fatalf("caller commit persisted domain effects=%d decisions=%d; want both one", effects, decisions)
	}
}
