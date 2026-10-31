//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestPostgresStaleRequestReconciliationIsIdempotent(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL service:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable PostgreSQL service:", err)
	}
	schemaName := fmt.Sprintf("kora_request_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated request schema:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`)
		_ = root.Close()
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse disposable PostgreSQL DSN:", err)
	}
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	database, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal("open isolated PostgreSQL request schema:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping isolated PostgreSQL request schema:", err)
	}
	runStaleRequestReconciliation(t, database, db.Resolve("postgres"), db.OutboxTablesPostgres())
}

func TestLibSQLRemoteStaleRequestReconciliationIsIdempotent(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN to a fresh disposable LibSQL HTTP database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP endpoint:", err)
	}
	database.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL HTTP endpoint:", err)
	}
	runStaleRequestReconciliation(t, database, db.Resolve("libsql"), db.OutboxTablesLibSQL())
}

func runStaleRequestReconciliation(t *testing.T, database *sql.DB, dialect db.Dialect, outboxSchema []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range outboxSchema {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create %s outbox schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_auth_revision (
		id INTEGER PRIMARY KEY CHECK (id = 1), policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL
	)`); err != nil {
		t.Fatalf("create %s request authorization revision: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_auth_revision (id, policy_revision, membership_revision) VALUES (1, 1, 1)`); err != nil {
		t.Fatalf("seed %s request authorization revision: %v", dialect.DriverName(), err)
	}
	statements, err := SchemaStatements(dialect)
	if err != nil {
		t.Fatal("get request schema:", err)
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create %s request schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	store, err := New(database, dialect)
	if err != nil {
		t.Fatal("construct request store:", err)
	}
	runDirectTaskNotificationRecipientLifecycle(t, ctx, database, dialect, store)
	runMultiAssignmentDecisionLifecycle(t, ctx, database, dialect, store)
	runAssignmentSpecificResponseLifecycle(t, ctx, database, dialect, store)
	runHistoryAndReadReceiptLifecycle(t, ctx, database, dialect, store)
	runConcurrentDecisionRace(t, ctx, database, dialect, store)
	runCancelDecisionRace(t, ctx, database, dialect, store)
	runExpiryDecisionRace(t, ctx, database, dialect, store)
	runCancelExpiryOrdering(t, ctx, database, dialect, store)
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := contract.RequestID(contract.NewID())
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: id, Revision: "1", Kind: contract.RequestApproval,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-dialect",
		Subject:       &contract.SubjectRef{Kind: "Invoice", ID: "invoice-dialect", Revision: "1"},
		SubjectDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Title:         "Changed invoice", Message: "Reconcile the subject update", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin request fixture:", err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatal("insert request fixture:", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit request fixture:", err)
	}
	observed := contract.SubjectRef{Kind: "Invoice", ID: "invoice-dialect", Revision: "2"}
	if err := store.MarkStale(ctx, record.Tenant, id, record.Revision, observed); err != nil {
		t.Fatalf("mark %s request stale: %v", dialect.DriverName(), err)
	}
	if err := store.MarkStale(ctx, record.Tenant, id, record.Revision, observed); err != nil {
		t.Fatalf("retry %s stale reconciliation: %v", dialect.DriverName(), err)
	}
	loaded, err := store.Get(ctx, record.Tenant, id)
	if err != nil || loaded.Status != contract.RequestStale || loaded.Revision != "2" {
		t.Fatalf("%s stale request after retry = status %q revision %q error %v", dialect.DriverName(), loaded.Status, loaded.Revision, err)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'stale'`:   1,
		`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type = 'request.stale'`: 1,
	} {
		var got int
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, query), string(record.Tenant), string(id)).Scan(&got); err != nil {
			t.Fatalf("count %s stale effects: %v", dialect.DriverName(), err)
		}
		if got != want {
			t.Errorf("%s stale effect count for %q = %d, want %d", dialect.DriverName(), query, got, want)
		}
	}
}

func runCancelExpiryOrdering(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	dueAt := now.Add(-time.Minute)
	requestID := contract.RequestID(contract.NewID())
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-dialect",
		Title: "Cancel after deadline", Message: "Deadline expiry takes precedence", DueAt: &dueAt, CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin cancel-expiry fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert cancel-expiry request: %v", dialect.DriverName(), err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit cancel-expiry fixture: %v", dialect.DriverName(), err)
	}
	cancellation := requests.CancelCommit{Identity: requests.WriteIdentity{Tenant: record.Tenant, Actor: record.Requester,
		Operation: requestCancelOperation, IdempotencyKey: contract.IdempotencyKey("cancel-expiry-" + contract.NewID()),
		CommandDigest: strings.Repeat("d", 64)}, RequestID: requestID, ExpectedRevision: "1",
		ExpectedPolicyRevision: "1", ExpectedMembershipRevision: "1", Reason: "No longer needed",
		Events: []contract.EventEnvelope{terminalEvent(record.Tenant, requestID, "request.cancelled", 2, now)}}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, err := store.Cancel(ctx, cancellation)
		results <- err
	}()
	go func() {
		defer workers.Done()
		<-start
		_, err := store.ExpireDue(ctx, record.Tenant, 1)
		results <- err
	}()
	close(start)
	workers.Wait()
	close(results)
	committed, conflicted := 0, 0
	for err := range results {
		if err == nil {
			committed++
			continue
		}
		var coded *contract.Error
		if errors.As(err, &coded) && coded.Type == contract.CodeConflict {
			conflicted++
			continue
		}
		t.Errorf("%s cancel/expiry ordering error: %v", dialect.DriverName(), err)
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("%s cancel/expiry outcomes committed=%d conflicted=%d; want expiry and a cancelled command conflict", dialect.DriverName(), committed, conflicted)
	}
	loaded, err := store.Get(ctx, record.Tenant, requestID)
	if err != nil || loaded.Status != contract.RequestExpired || loaded.Revision != "2" {
		t.Fatalf("%s cancel/expiry state=%s revision=%s err=%v; want expired revision 2", dialect.DriverName(), loaded.Status, loaded.Revision, err)
	}
	var expired, cancelled int
	for query, destination := range map[string]*int{
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'expired'`:   &expired,
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'cancelled'`: &cancelled,
	} {
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, query), string(record.Tenant), string(requestID)).Scan(destination); err != nil {
			t.Fatalf("%s count cancel-expiry history: %v", dialect.DriverName(), err)
		}
	}
	if expired != 1 || cancelled != 0 {
		t.Errorf("%s cancel/expiry history expired=%d cancelled=%d", dialect.DriverName(), expired, cancelled)
	}
}

func runHistoryAndReadReceiptLifecycle(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	requestID := contract.RequestID(contract.NewID())
	requester := contract.PrincipalID("requester-history")
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: requestID, Revision: "2", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: requester,
		Title: "Paged history", Message: "Read state is independent of completion", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin history fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert history request: %v", dialect.DriverName(), err)
	}
	actor := contract.PrincipalID(requester)
	for sequence, kind := range []contract.RequestHistoryKind{contract.HistoryCreated, contract.HistoryAssigned} {
		entry := contract.RequestHistoryEntry{Sequence: contract.Revision(strconv.Itoa(sequence + 1)), Kind: kind, Actor: &actor, Comment: "history item", At: now}
		if err := insertHistory(ctx, tx, dialect, record.Tenant, requestID, entry); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s insert history sequence %d: %v", dialect.DriverName(), sequence+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit history fixture: %v", dialect.DriverName(), err)
	}
	scope := requests.VisibilityScope{Tenant: record.Tenant, Actor: requester}
	first, err := store.History(ctx, scope, requestID, contract.PageRequest{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.Items[0].Sequence != "1" || first.NextCursor != "1" {
		t.Fatalf("%s first history page=%+v err=%v", dialect.DriverName(), first, err)
	}
	second, err := store.History(ctx, scope, requestID, contract.PageRequest{Limit: 1, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].Sequence != "2" || second.NextCursor != "" {
		t.Fatalf("%s second history page=%+v err=%v", dialect.DriverName(), second, err)
	}
	if err := store.MarkRead(ctx, scope, requestID); err != nil {
		t.Fatalf("%s mark request read: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_request SET revision = 3 WHERE tenant_id = ? AND id = ?`), string(record.Tenant), string(requestID)); err != nil {
		t.Fatalf("%s advance request revision for read watermark: %v", dialect.DriverName(), err)
	}
	if err := store.MarkRead(ctx, scope, requestID); err != nil {
		t.Fatalf("%s advance request read watermark: %v", dialect.DriverName(), err)
	}
	start := make(chan struct{})
	watermarkErrors := make(chan error, 8)
	var readers sync.WaitGroup
	for i := 0; i < cap(watermarkErrors); i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			watermarkErrors <- store.MarkRead(ctx, scope, requestID)
		}()
	}
	close(start)
	readers.Wait()
	close(watermarkErrors)
	for err := range watermarkErrors {
		if err != nil {
			t.Errorf("%s concurrent MarkRead: %v", dialect.DriverName(), err)
		}
	}
	var watermark int64
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT history_sequence FROM _kora_request_read WHERE request_id = ? AND principal_id = ?`), string(requestID), string(requester)).Scan(&watermark); err != nil {
		t.Fatalf("%s read request watermark: %v", dialect.DriverName(), err)
	}
	loaded, err := store.Get(ctx, record.Tenant, requestID)
	if err != nil || loaded.Status != contract.RequestPending || loaded.Revision != "3" || watermark != 3 {
		t.Fatalf("%s read receipt changed request status=%s revision=%s watermark=%d err=%v", dialect.DriverName(), loaded.Status, loaded.Revision, watermark, err)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ?`:                     2,
		`SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`:                    0,
		`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type LIKE 'request.%'`: 0,
	} {
		args := []any{string(record.Tenant), string(requestID)}
		if strings.Contains(query, "_kora_outbox") {
			args = []any{string(record.Tenant), string(requestID)}
		}
		var got int
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, query), args...).Scan(&got); err != nil {
			t.Fatalf("%s count read-receipt effects: %v", dialect.DriverName(), err)
		}
		if got != want {
			t.Errorf("%s read-receipt effect count for %q = %d, want %d", dialect.DriverName(), query, got, want)
		}
	}
}

func runAssignmentSpecificResponseLifecycle(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	actor := contract.PrincipalID("reviewer-allowed-response")
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-dialect",
		Title: "Assignment-specific response", Message: "Only one response is permitted", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin allowed-response fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert allowed-response request: %v", dialect.DriverName(), err)
	}
	assignment := requests.AssignmentRecord{ID: assignmentID, RequestID: requestID, Principal: actor,
		OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(actor)}, Stage: 1,
		Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
	if err := insertAssignment(ctx, tx, dialect, record.Tenant, assignment, now); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert allowed-response assignment: %v", dialect.DriverName(), err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit allowed-response fixture: %v", dialect.DriverName(), err)
	}
	makeChange := func(response contract.ResponseKind) requests.DecisionCommit {
		change := decisionTestCommit("any")
		change.Request = record
		change.ExpectedRevision, change.ExpectedPolicyRevision, change.ExpectedMembershipRevision = "1", "1", "1"
		change.Decision.ID, change.Decision.RequestID, change.Decision.AssignmentID = contract.DecisionID(contract.NewID()), requestID, assignmentID
		change.Decision.Actor, change.Decision.Tenant = actor, record.Tenant
		change.Decision.Response, change.Decision.Comment, change.Decision.At = response, "response", now
		change.Identity.Tenant, change.Identity.Actor = record.Tenant, actor
		change.Identity.IdempotencyKey = contract.IdempotencyKey("assignment-response-" + contract.NewID())
		change.Receipt = contract.ResponseReceipt{RequestID: requestID, DecisionID: change.Decision.ID}
		change.Events = []contract.EventEnvelope{{ID: contract.NewEventID(), Type: "request.responded", Version: contract.CurrentVersion,
			Source: "kora.engine", Site: "tenant-dialect-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now,
			Actor: contract.ActorContext{PrincipalID: string(actor), PrincipalType: contract.PrincipalHuman, Site: "tenant-dialect-route", AuthenticatedAt: now},
			Data: contract.MustEncodeData(struct {
				RequestID contract.RequestID `json:"request_id"`
				Revision  contract.Revision  `json:"revision"`
			}{requestID, "2"})}}
		return change
	}
	if _, err := store.CommitDecision(ctx, makeChange(contract.ResponseReturn)); err == nil {
		t.Fatalf("%s accepted response excluded by assignment's allowed_responses", dialect.DriverName())
	} else {
		var coded *contract.Error
		if !errors.As(err, &coded) || coded.Type != contract.CodePermissionDenied {
			t.Fatalf("%s disallowed response error=%v, want PERMISSION_DENIED", dialect.DriverName(), err)
		}
	}
	loaded, err := store.Get(ctx, record.Tenant, requestID)
	if err != nil || loaded.Status != contract.RequestPending || loaded.Revision != "1" {
		t.Fatalf("%s disallowed response mutated request status=%s revision=%s err=%v", dialect.DriverName(), loaded.Status, loaded.Revision, err)
	}
	var decisions, history, events int
	for query, destination := range map[string]*int{
		`SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`:                         &decisions,
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ?`:                          &history,
		`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type = 'request.responded'`: &events,
	} {
		args := []any{string(record.Tenant), string(requestID)}
		if strings.Contains(query, "_kora_outbox") {
			args = []any{"tenant-dialect-route", string(requestID)}
		}
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, query), args...).Scan(destination); err != nil {
			t.Fatalf("%s count disallowed response effects: %v", dialect.DriverName(), err)
		}
	}
	if decisions != 0 || history != 0 || events != 0 {
		t.Fatalf("%s disallowed response left decision=%d history=%d event=%d", dialect.DriverName(), decisions, history, events)
	}
	accepted, err := store.CommitDecision(ctx, makeChange(contract.ResponseComplete))
	if err != nil || accepted.Status != contract.RequestResolved || accepted.Revision != "2" {
		t.Fatalf("%s allowed response receipt=%+v err=%v", dialect.DriverName(), accepted, err)
	}
}

func runConcurrentDecisionRace(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	requestID := contract.RequestID(contract.NewID())
	assignmentIDs := []contract.AssignmentID{contract.AssignmentID(contract.NewID()), contract.AssignmentID(contract.NewID())}
	actors := []contract.PrincipalID{"reviewer-response-race-a", "reviewer-response-race-b"}
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAll, Status: contract.RequestPending, Requester: "requester-dialect",
		Title: "Concurrent reviews", Message: "Only one response may win this revision", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin concurrent-decision fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert concurrent-decision request: %v", dialect.DriverName(), err)
	}
	for i := range assignmentIDs {
		assignment := requests.AssignmentRecord{ID: assignmentIDs[i], RequestID: requestID, Principal: actors[i],
			OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(actors[i])}, Stage: 1,
			Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
		if err := insertAssignment(ctx, tx, dialect, record.Tenant, assignment, now); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s insert concurrent-decision assignment: %v", dialect.DriverName(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit concurrent-decision fixture: %v", dialect.DriverName(), err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for i := range assignmentIDs {
		change := decisionTestCommit("all")
		change.Request = record
		change.ExpectedRevision, change.ExpectedPolicyRevision, change.ExpectedMembershipRevision = "1", "1", "1"
		change.Decision.ID, change.Decision.RequestID, change.Decision.AssignmentID = contract.DecisionID(contract.NewID()), requestID, assignmentIDs[i]
		change.Decision.Actor, change.Decision.Tenant = actors[i], record.Tenant
		change.Decision.Response, change.Decision.Comment, change.Decision.At = contract.ResponseComplete, "completed", now
		change.Identity.Tenant, change.Identity.Actor = record.Tenant, actors[i]
		change.Identity.IdempotencyKey = contract.IdempotencyKey("decision-race-" + contract.NewID())
		change.Receipt = contract.ResponseReceipt{RequestID: requestID, DecisionID: change.Decision.ID}
		change.Events = []contract.EventEnvelope{{ID: contract.NewEventID(), Type: "request.responded", Version: contract.CurrentVersion,
			Source: "kora.engine", Site: "tenant-dialect-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now,
			Actor: contract.ActorContext{PrincipalID: string(actors[i]), PrincipalType: contract.PrincipalHuman, Site: "tenant-dialect-route", AuthenticatedAt: now},
			Data: contract.MustEncodeData(struct {
				RequestID contract.RequestID `json:"request_id"`
				Revision  contract.Revision  `json:"revision"`
			}{requestID, "2"})}}
		go func() {
			defer workers.Done()
			<-start
			_, err := store.CommitDecision(ctx, change)
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	committed, conflicted := 0, 0
	for err := range results {
		if err == nil {
			committed++
			continue
		}
		var coded *contract.Error
		if errors.As(err, &coded) && coded.Type == contract.CodeConflict {
			conflicted++
			continue
		}
		t.Errorf("%s competing response error: %v", dialect.DriverName(), err)
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("%s competing response outcomes committed=%d conflicted=%d; want one each", dialect.DriverName(), committed, conflicted)
	}
	loaded, err := store.Get(ctx, record.Tenant, requestID)
	if err != nil || loaded.Status != contract.RequestPending || loaded.Revision != "2" {
		t.Fatalf("%s competing response state=%s revision=%s err=%v; want pending revision 2", dialect.DriverName(), loaded.Status, loaded.Revision, err)
	}
	var decisions, responses, events int
	queries := []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`, &decisions},
		{`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'responded'`, &responses},
		{`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type = 'request.responded'`, &events},
	}
	for _, item := range queries {
		args := []any{string(record.Tenant), string(requestID)}
		if strings.Contains(item.query, "_kora_outbox") {
			args = []any{"tenant-dialect-route", string(requestID)}
		}
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, item.query), args...).Scan(item.dest); err != nil {
			t.Fatalf("%s count competing-response effects: %v", dialect.DriverName(), err)
		}
	}
	if decisions != 1 || responses != 1 || events != 1 {
		t.Errorf("%s competing response effects decisions=%d response_history=%d events=%d", dialect.DriverName(), decisions, responses, events)
	}
}

func runExpiryDecisionRace(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	dueAt := now.Add(-time.Minute)
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	actor := contract.PrincipalID("reviewer-expiry-race")
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-dialect",
		Title: "Expiry race", Message: "Expiry must fence response", DueAt: &dueAt, CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin expiry race fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert expiry race request: %v", dialect.DriverName(), err)
	}
	assignment := requests.AssignmentRecord{ID: assignmentID, RequestID: requestID, Principal: actor,
		OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(actor)}, Stage: 1,
		Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
	if err := insertAssignment(ctx, tx, dialect, record.Tenant, assignment, now); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert expiry race assignment: %v", dialect.DriverName(), err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit expiry race fixture: %v", dialect.DriverName(), err)
	}
	decision := decisionTestCommit("any")
	decision.Request = record
	decision.ExpectedRevision, decision.ExpectedPolicyRevision, decision.ExpectedMembershipRevision = "1", "1", "1"
	decision.Decision.ID, decision.Decision.RequestID, decision.Decision.AssignmentID = contract.DecisionID(contract.NewID()), requestID, assignmentID
	decision.Decision.Actor, decision.Decision.Tenant = actor, record.Tenant
	decision.Decision.Response, decision.Decision.Comment, decision.Decision.At = contract.ResponseComplete, "completed", now
	decision.Identity.Tenant, decision.Identity.Actor = record.Tenant, actor
	decision.Identity.IdempotencyKey = contract.IdempotencyKey("expiry-race-response-" + contract.NewID())
	decision.Receipt = contract.ResponseReceipt{RequestID: requestID, DecisionID: decision.Decision.ID}
	decision.Events = []contract.EventEnvelope{{ID: contract.NewEventID(), Type: "request.responded", Version: contract.CurrentVersion,
		Source: "kora.engine", Site: "tenant-dialect-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now,
		Actor: contract.ActorContext{PrincipalID: string(actor), PrincipalType: contract.PrincipalHuman, Site: "tenant-dialect-route", AuthenticatedAt: now},
		Data: contract.MustEncodeData(struct {
			RequestID contract.RequestID `json:"request_id"`
			Revision  contract.Revision  `json:"revision"`
		}{requestID, "2"})}}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, err := store.CommitDecision(ctx, decision)
		results <- err
	}()
	go func() {
		defer workers.Done()
		<-start
		_, err := store.ExpireDue(ctx, record.Tenant, 1)
		results <- err
	}()
	close(start)
	workers.Wait()
	close(results)
	committed, conflicted := 0, 0
	for err := range results {
		if err == nil {
			committed++
			continue
		}
		var coded *contract.Error
		if errors.As(err, &coded) && coded.Type == contract.CodeConflict {
			conflicted++
			continue
		}
		t.Errorf("%s expiry/decision race error: %v", dialect.DriverName(), err)
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("%s expiry/decision race outcomes committed=%d conflicted=%d; want one each", dialect.DriverName(), committed, conflicted)
	}
	loaded, err := store.Get(ctx, record.Tenant, requestID)
	if err != nil || loaded.Status != contract.RequestExpired || loaded.Revision != "2" {
		t.Fatalf("%s expiry race winner state=%s revision=%s err=%v; want expired revision 2", dialect.DriverName(), loaded.Status, loaded.Revision, err)
	}
	var decisions, expiredHistory, expiredEvents int
	for query, destination := range map[string]*int{
		`SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`:                       &decisions,
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'expired'`:   &expiredHistory,
		`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type = 'request.expired'`: &expiredEvents,
	} {
		args := []any{string(record.Tenant), string(requestID)}
		if strings.Contains(query, "_kora_outbox") {
			args = []any{string(record.Tenant), string(requestID)}
		}
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, query), args...).Scan(destination); err != nil {
			t.Fatalf("%s count expiry race effects: %v", dialect.DriverName(), err)
		}
	}
	if decisions != 0 || expiredHistory != 1 || expiredEvents != 1 {
		t.Errorf("%s expiry race effects decisions=%d expiry_history=%d expiry_events=%d", dialect.DriverName(), decisions, expiredHistory, expiredEvents)
	}
}

func runCancelDecisionRace(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	actor := contract.PrincipalID("reviewer-race")
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-dialect",
		Title: "Cancellation race", Message: "Either response or cancellation may win", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin cancellation race fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert cancellation race request: %v", dialect.DriverName(), err)
	}
	assignment := requests.AssignmentRecord{ID: assignmentID, RequestID: requestID, Principal: actor,
		OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(actor)}, Stage: 1,
		Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
	if err := insertAssignment(ctx, tx, dialect, record.Tenant, assignment, now); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert cancellation race assignment: %v", dialect.DriverName(), err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit cancellation race fixture: %v", dialect.DriverName(), err)
	}
	decision := decisionTestCommit("any")
	decision.Request = record
	decision.ExpectedRevision, decision.ExpectedPolicyRevision, decision.ExpectedMembershipRevision = "1", "1", "1"
	decision.Decision.ID, decision.Decision.RequestID, decision.Decision.AssignmentID = contract.DecisionID(contract.NewID()), requestID, assignmentID
	decision.Decision.Actor, decision.Decision.Tenant = actor, record.Tenant
	decision.Decision.Response, decision.Decision.Comment, decision.Decision.At = contract.ResponseComplete, "completed", now
	decision.Identity.Tenant, decision.Identity.Actor = record.Tenant, actor
	decision.Identity.IdempotencyKey = contract.IdempotencyKey("cancel-race-response-" + contract.NewID())
	decision.Receipt = contract.ResponseReceipt{RequestID: requestID, DecisionID: decision.Decision.ID}
	decision.Events = []contract.EventEnvelope{{ID: contract.NewEventID(), Type: "request.responded", Version: contract.CurrentVersion,
		Source: "kora.engine", Site: "tenant-dialect-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now,
		Actor: contract.ActorContext{PrincipalID: string(actor), PrincipalType: contract.PrincipalHuman, Site: "tenant-dialect-route", AuthenticatedAt: now},
		Data: contract.MustEncodeData(struct {
			RequestID contract.RequestID `json:"request_id"`
			Revision  contract.Revision  `json:"revision"`
		}{requestID, "2"})}}
	cancellation := requests.CancelCommit{Identity: requests.WriteIdentity{Tenant: record.Tenant, Actor: record.Requester,
		Operation: requestCancelOperation, IdempotencyKey: contract.IdempotencyKey("cancel-race-command-" + contract.NewID()),
		CommandDigest: strings.Repeat("c", 64)}, RequestID: requestID, ExpectedRevision: "1",
		ExpectedPolicyRevision: "1", ExpectedMembershipRevision: "1", Reason: "No longer needed",
		Events: []contract.EventEnvelope{terminalEvent(record.Tenant, requestID, "request.cancelled", 2, now)}}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, err := store.CommitDecision(ctx, decision)
		results <- err
	}()
	go func() {
		defer workers.Done()
		<-start
		_, err := store.Cancel(ctx, cancellation)
		results <- err
	}()
	close(start)
	workers.Wait()
	close(results)
	committed, conflicted := 0, 0
	for err := range results {
		if err == nil {
			committed++
			continue
		}
		var coded *contract.Error
		if errors.As(err, &coded) && coded.Type == contract.CodeConflict {
			conflicted++
			continue
		}
		t.Errorf("%s cancellation/decision race error: %v", dialect.DriverName(), err)
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("%s cancellation/decision race outcomes committed=%d conflicted=%d; want one each", dialect.DriverName(), committed, conflicted)
	}
	loaded, err := store.Get(ctx, record.Tenant, requestID)
	if err != nil || (loaded.Status != contract.RequestResolved && loaded.Status != contract.RequestCancelled) || loaded.Revision != "2" {
		t.Fatalf("%s cancellation/decision winner state=%s revision=%s err=%v", dialect.DriverName(), loaded.Status, loaded.Revision, err)
	}
	var decisions, cancellations int
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`), string(record.Tenant), string(requestID)).Scan(&decisions); err != nil {
		t.Fatalf("%s count race decisions: %v", dialect.DriverName(), err)
	}
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'cancelled'`), string(record.Tenant), string(requestID)).Scan(&cancellations); err != nil {
		t.Fatalf("%s count race cancellations: %v", dialect.DriverName(), err)
	}
	if loaded.Status == contract.RequestResolved && (decisions != 1 || cancellations != 0) {
		t.Errorf("%s response winner left decisions=%d cancellation-history=%d", dialect.DriverName(), decisions, cancellations)
	}
	if loaded.Status == contract.RequestCancelled && (decisions != 0 || cancellations != 1) {
		t.Errorf("%s cancellation winner left decisions=%d cancellation-history=%d", dialect.DriverName(), decisions, cancellations)
	}
}

func runDirectTaskNotificationRecipientLifecycle(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_user (name TEXT NOT NULL, site TEXT NOT NULL, enabled SMALLINT NOT NULL, PRIMARY KEY (site, name))`); err != nil {
		t.Fatalf("%s create tenant user fixture: %v", dialect.DriverName(), err)
	}
	for _, user := range []struct {
		name    string
		enabled bool
	}{{"requester-dialect", true}, {"reviewer-a", true}, {"reviewer-b", false}} {
		if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_user (name, site, enabled) VALUES (?, ?, ?)`), user.name, "tenant-dialect-route", db.BoolParam(dialect, user.enabled)); err != nil {
			t.Fatalf("%s seed tenant user %s: %v", dialect.DriverName(), user.name, err)
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	directID := contract.RequestID(contract.NewID())
	direct := requests.RequestRecord{Tenant: "tenant-dialect", ID: directID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentSequential, Status: contract.RequestPending, Requester: "requester-dialect",
		Title: "Direct task", Message: "Complete in order", CreatedAt: now, UpdatedAt: now}
	approvalID := contract.RequestID(contract.NewID())
	approval := requests.RequestRecord{Tenant: "tenant-dialect", ID: approvalID, Revision: "1", Kind: contract.RequestApproval,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "requester-dialect",
		Subject: &contract.SubjectRef{Kind: "Invoice", ID: "invoice-notify", Revision: "1"}, PolicyID: "invoice-approval-v1",
		Title: "Governed approval", Message: "Approve invoice", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin notification fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, direct); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert direct notification request: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, approval); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert approval notification request: %v", dialect.DriverName(), err)
	}
	for _, item := range []struct {
		id     contract.AssignmentID
		actor  contract.PrincipalID
		stage  uint16
		status contract.AssignmentStatus
	}{{contract.AssignmentID(contract.NewID()), "reviewer-a", 1, contract.AssignmentPending},
		{contract.AssignmentID(contract.NewID()), "reviewer-b", 2, contract.AssignmentBlocked}} {
		assignment := requests.AssignmentRecord{ID: item.id, RequestID: direct.ID, Principal: item.actor,
			OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(item.actor)}, Stage: item.stage,
			Status: item.status, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
		if err := insertAssignment(ctx, tx, dialect, direct.Tenant, assignment, now); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s insert direct notification assignment: %v", dialect.DriverName(), err)
		}
	}
	approvalAssignment := requests.AssignmentRecord{ID: contract.AssignmentID(contract.NewID()), RequestID: approval.ID, Principal: "reviewer-a",
		OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "reviewer-a"}, Stage: 1, Status: contract.AssignmentPending,
		AllowedResponses: []contract.ResponseKind{contract.ResponseApprove}, MembershipRevision: "1"}
	if err := insertAssignment(ctx, tx, dialect, approval.Tenant, approvalAssignment, now); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert governed approval assignment: %v", dialect.DriverName(), err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit notification fixture: %v", dialect.DriverName(), err)
	}
	created, err := store.DirectTaskNotificationRecipients(ctx, direct.Tenant, "tenant-dialect-route", direct.ID, "request.created")
	if err != nil || !reflect.DeepEqual(created, []contract.PrincipalID{"reviewer-a"}) {
		t.Fatalf("%s direct-task created recipients=%v err=%v; want active sequential assignee only", dialect.DriverName(), created, err)
	}
	responded, err := store.DirectTaskNotificationRecipients(ctx, direct.Tenant, "tenant-dialect-route", direct.ID, "request.responded")
	if err != nil || !reflect.DeepEqual(responded, []contract.PrincipalID{"requester-dialect", "reviewer-a"}) {
		t.Fatalf("%s direct-task response recipients=%v err=%v; want requester and active pending participant", dialect.DriverName(), responded, err)
	}
	terminal, err := store.DirectTaskNotificationRecipients(ctx, direct.Tenant, "tenant-dialect-route", direct.ID, "request.cancelled")
	if err != nil || !reflect.DeepEqual(terminal, []contract.PrincipalID{"requester-dialect", "reviewer-a"}) {
		t.Fatalf("%s direct-task terminal recipients=%v err=%v; want active participants only", dialect.DriverName(), terminal, err)
	}
	governed, err := store.DirectTaskNotificationRecipients(ctx, approval.Tenant, "tenant-dialect-route", approval.ID, "request.created")
	if err != nil || len(governed) != 0 {
		t.Fatalf("%s governed approval recipients=%v err=%v; want no direct-task recipients", dialect.DriverName(), governed, err)
	}
}

func runMultiAssignmentDecisionLifecycle(t *testing.T, ctx context.Context, database *sql.DB, dialect db.Dialect, store *Store) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	requestID := contract.RequestID(contract.NewID())
	assignmentA := contract.AssignmentID(contract.NewID())
	assignmentB := contract.AssignmentID(contract.NewID())
	actorA, actorB := contract.PrincipalID("reviewer-a"), contract.PrincipalID("reviewer-b")
	record := requests.RequestRecord{Tenant: "tenant-dialect", ID: requestID, Revision: "1", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAll, Status: contract.RequestPending, Requester: "requester-dialect",
		Title: "Two reviews", Message: "Both reviewers must complete", CreatedAt: now, UpdatedAt: now}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("%s begin all-quorum fixture: %v", dialect.DriverName(), err)
	}
	if err := insertRequest(ctx, tx, dialect, record); err != nil {
		_ = tx.Rollback()
		t.Fatalf("%s insert all-quorum request: %v", dialect.DriverName(), err)
	}
	for _, item := range []struct {
		id    contract.AssignmentID
		actor contract.PrincipalID
	}{{assignmentA, actorA}, {assignmentB, actorB}} {
		assignment := requests.AssignmentRecord{ID: item.id, RequestID: requestID, Principal: item.actor,
			OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(item.actor)}, Stage: 1,
			Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}
		if err := insertAssignment(ctx, tx, dialect, record.Tenant, assignment, now); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s insert all-quorum assignment: %v", dialect.DriverName(), err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("%s commit all-quorum fixture: %v", dialect.DriverName(), err)
	}
	var finalChange requests.DecisionCommit
	var finalReceipt contract.ResponseReceipt
	for i, item := range []struct {
		assignment contract.AssignmentID
		actor      contract.PrincipalID
	}{{assignmentA, actorA}, {assignmentB, actorB}} {
		change := decisionTestCommit("all")
		change.Request = record
		change.ExpectedRevision, change.ExpectedPolicyRevision, change.ExpectedMembershipRevision = contract.Revision(strconv.Itoa(i+1)), "1", "1"
		change.Request.Revision = change.ExpectedRevision
		change.Decision.ID, change.Decision.RequestID, change.Decision.AssignmentID = contract.DecisionID(contract.NewID()), requestID, item.assignment
		change.Decision.Actor, change.Decision.Tenant = item.actor, record.Tenant
		change.Decision.Response, change.Decision.Comment, change.Decision.At = contract.ResponseComplete, fmt.Sprintf("review-%d", i+1), now
		change.Identity.Tenant, change.Identity.Actor = record.Tenant, item.actor
		change.Identity.IdempotencyKey = contract.IdempotencyKey("dialect-response-" + contract.NewID())
		change.Receipt = contract.ResponseReceipt{RequestID: requestID, DecisionID: change.Decision.ID}
		nextRevision := contract.Revision(strconv.Itoa(i + 2))
		change.Events = []contract.EventEnvelope{{ID: contract.NewEventID(), Type: "request.responded", Version: contract.CurrentVersion,
			Source: "kora.engine", Site: "tenant-dialect-route", AggregateType: "request", AggregateID: string(requestID), OccurredAt: now,
			Actor: contract.ActorContext{PrincipalID: string(item.actor), PrincipalType: contract.PrincipalHuman, Site: "tenant-dialect-route", AuthenticatedAt: now},
			Data: contract.MustEncodeData(struct {
				RequestID contract.RequestID `json:"request_id"`
				Revision  contract.Revision  `json:"revision"`
			}{requestID, nextRevision})}}
		receipt, err := store.CommitDecision(ctx, change)
		if err != nil {
			t.Fatalf("%s commit quorum decision %d: %v", dialect.DriverName(), i+1, err)
		}
		wantStatus := contract.RequestPending
		if i == 1 {
			wantStatus = contract.RequestResolved
		}
		if receipt.Status != wantStatus || receipt.Revision != nextRevision {
			t.Fatalf("%s quorum decision %d receipt=%+v; want status %s revision %s", dialect.DriverName(), i+1, receipt, wantStatus, nextRevision)
		}
		finalChange, finalReceipt = change, receipt
	}
	replayed, err := store.CommitDecision(ctx, finalChange)
	if err != nil || !reflect.DeepEqual(replayed, finalReceipt) {
		t.Fatalf("%s replay final quorum receipt=%+v err=%v, want %+v", dialect.DriverName(), replayed, err, finalReceipt)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM _kora_request_decision WHERE tenant_id = ? AND request_id = ?`:                                          2,
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'responded'`:                    2,
		`SELECT COUNT(*) FROM _kora_request_history WHERE tenant_id = ? AND request_id = ? AND kind = 'resolved'`:                     1,
		`SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ? AND event_type = 'request.responded'`:                  2,
		`SELECT COUNT(*) FROM _kora_request_idempotency WHERE tenant_id = ? AND operation = 'request.respond' AND actor_id IN (?, ?)`: 2,
	} {
		var got int
		args := []any{string(record.Tenant), string(requestID)}
		if strings.Contains(query, "_kora_outbox") {
			args = []any{"tenant-dialect-route", string(requestID)}
		} else if strings.Contains(query, "actor_id IN") {
			args = []any{string(record.Tenant), string(actorA), string(actorB)}
		}
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, query), args...).Scan(&got); err != nil {
			t.Fatalf("%s count quorum effects: %v", dialect.DriverName(), err)
		}
		if got != want {
			t.Errorf("%s quorum effect count for %q = %d, want %d", dialect.DriverName(), query, got, want)
		}
	}
}
