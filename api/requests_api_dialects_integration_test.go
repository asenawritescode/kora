//go:build integration

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
	requestsql "github.com/asenawritescode/kora/requests/sqlstore"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestPostgresDirectTaskRequestHTTPFlow(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable PostgreSQL:", err)
	}
	schemaName := fmt.Sprintf("kora_request_api_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated API schema:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`); _ = root.Close() })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse disposable DSN:", err)
	}
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	database, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal("open isolated schema:", err)
	}
	defer database.Close()
	runDirectTaskRequestHTTPFlow(t, database, db.Resolve("postgres"), db.OutboxTablesPostgres())
}

func TestLibSQLRemoteDirectTaskRequestHTTPFlow(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN to a fresh disposable LibSQL HTTP database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP endpoint:", err)
	}
	defer database.Close()
	runDirectTaskRequestHTTPFlow(t, database, db.Resolve("libsql"), db.OutboxTablesLibSQL())
}

func runDirectTaskRequestHTTPFlow(t *testing.T, database *sql.DB, dialect db.Dialect, outboxDDL []string) {
	t.Helper()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range outboxDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %s outbox schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	requestDDL, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	for _, statement := range requestDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply %s request schema: %v\n%s", dialect.DriverName(), err, statement)
		}
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_user (site TEXT NOT NULL, name TEXT NOT NULL, enabled SMALLINT NOT NULL, PRIMARY KEY(site,name))`); err != nil {
		t.Fatalf("%s create user table: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_auth_revision (id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL)`); err != nil {
		t.Fatalf("%s create authorization revision: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_config_revision (id INTEGER PRIMARY KEY, revision BIGINT NOT NULL)`); err != nil {
		t.Fatalf("%s create tenant config revision: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_user (site,name,enabled) VALUES (?,?,?),(?,?,?),(?,?,?)`), "tenant-route", "alice", 1, "tenant-route", "bob", 1, "tenant-route", "charlie", 1); err != nil {
		t.Fatalf("%s insert active users: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_auth_revision (id,policy_revision,membership_revision) VALUES (1,1,1)`); err != nil {
		t.Fatalf("%s seed authorization revision: %v", dialect.DriverName(), err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_config_revision (id,revision) VALUES (1,1)`); err != nil {
		t.Fatalf("%s seed tenant config revision: %v", dialect.DriverName(), err)
	}
	store, err := requestsql.New(database, dialect)
	if err != nil {
		t.Fatal("construct request store:", err)
	}
	authorizer, err := requestsql.NewDirectTaskAuthorizer(database, dialect)
	if err != nil {
		t.Fatal("construct direct-task authorizer:", err)
	}
	service, err := requests.NewService(store, authorizer, requests.ParticipantProjector{}, requests.NoResponseSchemas{}, apiRequestClock{}, apiRequestIDs{})
	if err != nil {
		t.Fatal("construct request service:", err)
	}
	routerFor := func(user string) *gin.Engine {
		gin.SetMode(gin.TestMode)
		router := gin.New()
		router.Use(func(c *gin.Context) {
			execution, err := policy.NewVerifiedExecution("tenant-http-dialects", contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user, Site: "tenant-route", AuthenticatedAt: time.Now().UTC()})
			if err != nil {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			c.Set("site_name", "tenant-route")
			c.Set("site_runtime_services", SiteRuntimeService{Requests: service})
			c.Set("policy_execution", execution)
			c.Next()
		})
		RegisterRequestRoutes(router.Group("/api/v2"), nil)
		return router
	}
	alice, bob, charlie := routerFor("alice"), routerFor("bob"), routerFor("charlie")
	submitKey := contract.IdempotencyKey(contract.NewID())
	body, err := json.Marshal(contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: submitKey}, Kind: contract.RequestTask,
		Title: "Review order", Message: "Please complete this review", Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny,
			Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "bob"}}}})
	if err != nil {
		t.Fatal("encode request:", err)
	}
	created := httptest.NewRecorder()
	alice.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/v2/requests", bytes.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("%s request submit status=%d body=%s", dialect.DriverName(), created.Code, created.Body.String())
	}
	var createdEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdEnvelope); err != nil || createdEnvelope.Data.ID == "" || len(createdEnvelope.Data.Assignments) != 1 {
		t.Fatalf("%s created request=%+v err=%v", dialect.DriverName(), createdEnvelope.Data, err)
	}
	requestPath := "/api/v2/requests/" + string(createdEnvelope.Data.ID)
	inbox := httptest.NewRecorder()
	bob.ServeHTTP(inbox, httptest.NewRequest(http.MethodGet, "/api/v2/requests?view=needs_action", nil))
	var inboxEnvelope struct {
		Data contract.Page[contract.RequestView] `json:"data"`
	}
	if inbox.Code != http.StatusOK || json.Unmarshal(inbox.Body.Bytes(), &inboxEnvelope) != nil || len(inboxEnvelope.Data.Items) != 1 || inboxEnvelope.Data.Items[0].ID != createdEnvelope.Data.ID {
		t.Fatalf("%s assignee inbox status=%d body=%s", dialect.DriverName(), inbox.Code, inbox.Body.String())
	}
	assignment := inboxEnvelope.Data.Items[0].Assignments[0]
	responseKey := contract.IdempotencyKey(contract.NewID())
	responseBody, err := json.Marshal(contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: responseKey, ExpectedRevision: inboxEnvelope.Data.Items[0].Revision},
		AssignmentID: assignment.ID, Response: contract.ResponseComplete})
	if err != nil {
		t.Fatal("encode response:", err)
	}
	responded := httptest.NewRecorder()
	bob.ServeHTTP(responded, httptest.NewRequest(http.MethodPost, requestPath+"/responses", bytes.NewReader(responseBody)))
	if responded.Code != http.StatusOK {
		t.Fatalf("%s response status=%d body=%s", dialect.DriverName(), responded.Code, responded.Body.String())
	}
	var receiptEnvelope struct {
		Data contract.ResponseReceipt `json:"data"`
	}
	if err := json.Unmarshal(responded.Body.Bytes(), &receiptEnvelope); err != nil || receiptEnvelope.Data.Status != contract.RequestResolved || receiptEnvelope.Data.Outcome == nil || *receiptEnvelope.Data.Outcome != contract.OutcomeCompleted {
		t.Fatalf("%s response receipt=%+v err=%v", dialect.DriverName(), receiptEnvelope.Data, err)
	}
	requesterView := httptest.NewRecorder()
	alice.ServeHTTP(requesterView, httptest.NewRequest(http.MethodGet, requestPath, nil))
	var requesterEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if requesterView.Code != http.StatusOK || json.Unmarshal(requesterView.Body.Bytes(), &requesterEnvelope) != nil || requesterEnvelope.Data.Status != contract.RequestResolved {
		t.Fatalf("%s requester view status=%d body=%s", dialect.DriverName(), requesterView.Code, requesterView.Body.String())
	}
	history := httptest.NewRecorder()
	alice.ServeHTTP(history, httptest.NewRequest(http.MethodGet, requestPath+"/history", nil))
	var historyEnvelope struct {
		Data contract.Page[contract.RequestHistoryEntry] `json:"data"`
	}
	if history.Code != http.StatusOK || json.Unmarshal(history.Body.Bytes(), &historyEnvelope) != nil || len(historyEnvelope.Data.Items) < 3 {
		t.Fatalf("%s requester history status=%d body=%s", dialect.DriverName(), history.Code, history.Body.String())
	}
	responseDetail := httptest.NewRecorder()
	alice.ServeHTTP(responseDetail, httptest.NewRequest(http.MethodGet, requestPath+"/responses/"+string(receiptEnvelope.Data.DecisionID), nil))
	var detailEnvelope struct {
		Data contract.ResponseDetail `json:"data"`
	}
	if responseDetail.Code != http.StatusOK || json.Unmarshal(responseDetail.Body.Bytes(), &detailEnvelope) != nil || detailEnvelope.Data.Response != contract.ResponseComplete {
		t.Fatalf("%s requester response detail status=%d body=%s", dialect.DriverName(), responseDetail.Code, responseDetail.Body.String())
	}
	markedRead := httptest.NewRecorder()
	alice.ServeHTTP(markedRead, httptest.NewRequest(http.MethodPost, requestPath+"/read", nil))
	if markedRead.Code != http.StatusNoContent {
		t.Fatalf("%s mark read status=%d body=%s", dialect.DriverName(), markedRead.Code, markedRead.Body.String())
	}
	var readReceipts int
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_request_read WHERE request_id=? AND principal_id='alice'`), string(createdEnvelope.Data.ID)).Scan(&readReceipts); err != nil || readReceipts != 1 {
		t.Fatalf("%s read receipts=%d err=%v, want one requester receipt", dialect.DriverName(), readReceipts, err)
	}

	secondBody, err := json.Marshal(contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(contract.NewID())}, Kind: contract.RequestTask,
		Title: "Reassign work", Message: "Move this task to the next reviewer", Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny,
			Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "bob"}}}})
	if err != nil {
		t.Fatal("encode second request:", err)
	}
	secondCreated := httptest.NewRecorder()
	alice.ServeHTTP(secondCreated, httptest.NewRequest(http.MethodPost, "/api/v2/requests", bytes.NewReader(secondBody)))
	var secondEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if secondCreated.Code != http.StatusCreated || json.Unmarshal(secondCreated.Body.Bytes(), &secondEnvelope) != nil || len(secondEnvelope.Data.Assignments) != 1 {
		t.Fatalf("%s second request status=%d body=%s", dialect.DriverName(), secondCreated.Code, secondCreated.Body.String())
	}
	secondPath := "/api/v2/requests/" + string(secondEnvelope.Data.ID)
	reassignBody, err := json.Marshal(contract.ReassignRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(contract.NewID()), ExpectedRevision: secondEnvelope.Data.Revision},
		AssignmentID: secondEnvelope.Data.Assignments[0].ID, To: contract.Assignee{Kind: contract.AssigneeUser, ID: "charlie"}, Reason: "Coverage changed"})
	if err != nil {
		t.Fatal("encode reassignment:", err)
	}
	reassigned := httptest.NewRecorder()
	alice.ServeHTTP(reassigned, httptest.NewRequest(http.MethodPost, secondPath+"/reassignments", bytes.NewReader(reassignBody)))
	var reassignedEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if reassigned.Code != http.StatusOK || json.Unmarshal(reassigned.Body.Bytes(), &reassignedEnvelope) != nil || len(reassignedEnvelope.Data.Assignments) != 1 || reassignedEnvelope.Data.Assignments[0].Assignee.ID != "charlie" {
		t.Fatalf("%s reassignment status=%d body=%s", dialect.DriverName(), reassigned.Code, reassigned.Body.String())
	}
	charlieInbox := httptest.NewRecorder()
	charlie.ServeHTTP(charlieInbox, httptest.NewRequest(http.MethodGet, "/api/v2/requests?view=needs_action", nil))
	var charlieInboxEnvelope struct {
		Data contract.Page[contract.RequestView] `json:"data"`
	}
	if charlieInbox.Code != http.StatusOK || json.Unmarshal(charlieInbox.Body.Bytes(), &charlieInboxEnvelope) != nil || len(charlieInboxEnvelope.Data.Items) != 1 || charlieInboxEnvelope.Data.Items[0].ID != secondEnvelope.Data.ID {
		t.Fatalf("%s reassigned user's inbox status=%d body=%s", dialect.DriverName(), charlieInbox.Code, charlieInbox.Body.String())
	}
	cancelBody, err := json.Marshal(contract.CancelRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(contract.NewID()), ExpectedRevision: reassignedEnvelope.Data.Revision}, Reason: "Request withdrawn"})
	if err != nil {
		t.Fatal("encode cancellation:", err)
	}
	cancelled := httptest.NewRecorder()
	alice.ServeHTTP(cancelled, httptest.NewRequest(http.MethodPost, secondPath+"/cancel", bytes.NewReader(cancelBody)))
	var cancelledEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if cancelled.Code != http.StatusOK || json.Unmarshal(cancelled.Body.Bytes(), &cancelledEnvelope) != nil || cancelledEnvelope.Data.Status != contract.RequestCancelled {
		t.Fatalf("%s cancellation status=%d body=%s", dialect.DriverName(), cancelled.Code, cancelled.Body.String())
	}
}
