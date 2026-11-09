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
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

type apiRequestClock struct{}

func (apiRequestClock) Now() time.Time { return time.Now().UTC() }

type apiRequestIDs struct{}

func (apiRequestIDs) NewID() string { return contract.NewID() }

// TestMySQLDirectTaskHTTPFlowWithoutCloudOrBroker exercises the authenticated
// request API with only a tenant MySQL database and Engine-owned request
// services composed. No Cloud or broker process is configured in this path.
func TestMySQLDirectTaskHTTPFlowWithoutCloudOrBroker(t *testing.T) {
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
	name := fmt.Sprintf("kora_request_http_%d", time.Now().UnixNano()%1_000_000_000)
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
	for _, ddl := range append([]string{
		`CREATE TABLE _kora_user (name VARCHAR(140) PRIMARY KEY, site VARCHAR(140) NOT NULL, enabled TINYINT(1) NOT NULL DEFAULT 1, password_hash VARCHAR(255) NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE _kora_auth_revision (id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL) ENGINE=InnoDB`,
		`CREATE TABLE _kora_config_revision (id INTEGER PRIMARY KEY, revision BIGINT NOT NULL) ENGINE=InnoDB`,
	}, mustRequestSchema(t, db.Resolve("mysql"))...) {
		if _, err := database.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("apply request test schema: %v\n%s", err, ddl)
		}
	}
	for _, ddl := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, ddl); err != nil {
			t.Fatal("apply outbox schema:", err)
		}
	}
	for _, user := range []string{"alice", "bob", "charlie"} {
		if _, err := database.ExecContext(ctx, `INSERT INTO _kora_user (name,site,enabled,password_hash) VALUES (?,?,1,'test')`, user, "acme"); err != nil {
			t.Fatal("seed test user:", err)
		}
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_auth_revision (id,policy_revision,membership_revision) VALUES (1,1,1)`); err != nil {
		t.Fatal("seed authorization revision:", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_config_revision (id,revision) VALUES (1,1)`); err != nil {
		t.Fatal("seed tenant config revision:", err)
	}
	store, err := requestsql.New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := requestsql.NewDirectTaskAuthorizer(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := requests.NewService(store, authorizer, requests.DirectTaskProjector{}, requests.NoResponseSchemas{}, apiRequestClock{}, apiRequestIDs{})
	if err != nil {
		t.Fatal(err)
	}

	routerFor := func(user string) *gin.Engine {
		gin.SetMode(gin.TestMode)
		router := gin.New()
		router.Use(func(c *gin.Context) {
			execution, err := policy.NewVerifiedExecution("tenant-http-test", contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user, Site: "acme", AuthenticatedAt: time.Now().UTC()})
			if err != nil {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			c.Set("site_name", "acme")
			c.Set("site_runtime_services", SiteRuntimeService{Requests: service})
			c.Set("policy_execution", execution)
			c.Next()
		})
		RegisterRequestRoutes(router.Group("/api/v2"), nil)
		return router
	}
	alice, bob := routerFor("alice"), routerFor("bob")
	key := contract.NewID()
	body, _ := json.Marshal(contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(key)}, Kind: contract.RequestTask, Title: "Review order", Message: "Please complete this review", Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAll, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "bob"}, {Kind: contract.AssigneeUser, ID: "charlie"}}}})
	created := httptest.NewRecorder()
	alice.ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/v2/requests", bytes.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("submit status=%d body=%s", created.Code, created.Body.String())
	}
	var createdEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdEnvelope); err != nil {
		t.Fatal("decode created request:", err)
	}
	if createdEnvelope.Data.ID == "" || len(createdEnvelope.Data.Assignments) != 2 {
		t.Fatalf("created projection = %#v", createdEnvelope.Data)
	}

	inbox := httptest.NewRecorder()
	bob.ServeHTTP(inbox, httptest.NewRequest(http.MethodGet, "/api/v2/requests?view=needs_action", nil))
	if inbox.Code != http.StatusOK {
		t.Fatalf("inbox status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	var inboxEnvelope struct {
		Data contract.Page[contract.RequestView] `json:"data"`
	}
	if err := json.Unmarshal(inbox.Body.Bytes(), &inboxEnvelope); err != nil {
		t.Fatal("decode inbox:", err)
	}
	if len(inboxEnvelope.Data.Items) != 1 || inboxEnvelope.Data.Items[0].ID != createdEnvelope.Data.ID || len(inboxEnvelope.Data.Items[0].Assignments) != 1 {
		t.Fatalf("recipient inbox projection = %#v", inboxEnvelope.Data)
	}
	assignment := inboxEnvelope.Data.Items[0].Assignments[0]
	responseKey := contract.NewID()
	responseBody, _ := json.Marshal(contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(responseKey), ExpectedRevision: "1"}, AssignmentID: assignment.ID, Response: contract.ResponseComplete, Comment: "private-bob-response"})
	path := "/api/v2/requests/" + string(createdEnvelope.Data.ID) + "/responses"
	responded := httptest.NewRecorder()
	bob.ServeHTTP(responded, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(responseBody)))
	if responded.Code != http.StatusOK {
		t.Fatalf("respond status=%d body=%s", responded.Code, responded.Body.String())
	}
	var responseEnvelope struct {
		Data contract.ResponseReceipt `json:"data"`
	}
	if err := json.Unmarshal(responded.Body.Bytes(), &responseEnvelope); err != nil {
		t.Fatal("decode response:", err)
	}
	if responseEnvelope.Data.Status != contract.RequestPending || responseEnvelope.Data.DecisionID == "" {
		t.Fatalf("response receipt = %#v", responseEnvelope.Data)
	}
	charlie := routerFor("charlie")
	requestPath := "/api/v2/requests/" + string(createdEnvelope.Data.ID)
	charlieView := httptest.NewRecorder()
	charlie.ServeHTTP(charlieView, httptest.NewRequest(http.MethodGet, requestPath, nil))
	if charlieView.Code != http.StatusOK {
		t.Fatalf("other assignee request status=%d body=%s", charlieView.Code, charlieView.Body.String())
	}
	var charlieEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if err := json.Unmarshal(charlieView.Body.Bytes(), &charlieEnvelope); err != nil {
		t.Fatal("decode other assignee request:", err)
	}
	if len(charlieEnvelope.Data.Assignments) != 1 || charlieEnvelope.Data.Assignments[0].Assignee.ID != "charlie" {
		t.Fatalf("other assignee saw another assignment: %#v", charlieEnvelope.Data.Assignments)
	}
	charlieHistory := httptest.NewRecorder()
	charlie.ServeHTTP(charlieHistory, httptest.NewRequest(http.MethodGet, requestPath+"/history", nil))
	if charlieHistory.Code != http.StatusOK {
		t.Fatalf("other assignee history status=%d body=%s", charlieHistory.Code, charlieHistory.Body.String())
	}
	var historyEnvelope struct {
		Data contract.Page[contract.RequestHistoryEntry] `json:"data"`
	}
	if err := json.Unmarshal(charlieHistory.Body.Bytes(), &historyEnvelope); err != nil {
		t.Fatal("decode other assignee history:", err)
	}
	var hiddenOtherResponse bool
	for _, entry := range historyEnvelope.Data.Items {
		if entry.Kind == contract.HistoryResponded {
			hiddenOtherResponse = entry.Actor == nil && entry.AssignmentID == "" && entry.Response == nil && entry.Comment == ""
		}
	}
	if !hiddenOtherResponse {
		t.Fatalf("other assignee history exposed response details: %+v", historyEnvelope.Data.Items)
	}
	otherResponsePath := requestPath + "/responses/" + string(responseEnvelope.Data.DecisionID)
	otherResponse := httptest.NewRecorder()
	charlie.ServeHTTP(otherResponse, httptest.NewRequest(http.MethodGet, otherResponsePath, nil))
	if otherResponse.Code != http.StatusNotFound {
		t.Fatalf("other assignee response details status=%d body=%s; want 404", otherResponse.Code, otherResponse.Body.String())
	}
	requesterResponse := httptest.NewRecorder()
	alice.ServeHTTP(requesterResponse, httptest.NewRequest(http.MethodGet, otherResponsePath, nil))
	if requesterResponse.Code != http.StatusOK || !bytes.Contains(requesterResponse.Body.Bytes(), []byte("private-bob-response")) {
		t.Fatalf("requester response details status=%d body=%s", requesterResponse.Code, requesterResponse.Body.String())
	}
}

func mustRequestSchema(t *testing.T, dialect db.Dialect) []string {
	t.Helper()
	statements, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal(err)
	}
	return statements
}
