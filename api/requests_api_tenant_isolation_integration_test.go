//go:build integration

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
	requestsql "github.com/asenawritescode/kora/requests/sqlstore"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

// TestMySQLRequestHTTPIsolationAcrossTenantDatabases verifies that an HTTP
// request ID from one tenant database cannot be read or acted on through
// another tenant's verified execution and request service. It requires an
// explicitly named disposable, loopback-only MySQL DSN; it never uses the
// general integration-test or application database settings.
func TestMySQLRequestHTTPIsolationAcrossTenantDatabases(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_REQUEST_ISOLATION_DISPOSABLE_DSN"))
	if dsn == "" {
		t.Skip("set KORA_REQUEST_ISOLATION_DISPOSABLE_DSN to a disposable loopback MySQL service")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse disposable MySQL DSN: %v", err)
	}
	host, _, err := net.SplitHostPort(rootConfig.Addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("disposable MySQL DSN must use a loopback TCP address; host=%q err=%v", host, err)
	}
	rootConfig.DBName = ""
	root, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL server:", err)
	}
	root.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = root.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := root.PingContext(ctx); err != nil {
		t.Fatalf("ping disposable MySQL server: %v", err)
	}

	databaseNames := []string{
		"kora_req_iso_a_" + strings.ToLower(contract.NewID()),
		"kora_req_iso_b_" + strings.ToLower(contract.NewID()),
	}
	dialect := db.Resolve("mysql")
	for _, databaseName := range databaseNames {
		if _, err := root.ExecContext(ctx, "CREATE DATABASE `"+databaseName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatalf("create disposable tenant database: %v", err)
		}
		name := databaseName
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			if _, err := root.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
				t.Errorf("drop test-owned tenant database %q: %v", name, err)
			}
		})
	}

	tenantRouters := make([]*gin.Engine, 0, len(databaseNames))
	for index, databaseName := range databaseNames {
		cfg := *rootConfig
		cfg.DBName = databaseName
		database, err := sql.Open("mysql", cfg.FormatDSN())
		if err != nil {
			t.Fatalf("open tenant database %q: %v", databaseName, err)
		}
		database.SetMaxOpenConns(4)
		databaseName := databaseName
		t.Cleanup(func() { _ = database.Close() })
		for _, statement := range append([]string{
			`CREATE TABLE _kora_user (name VARCHAR(140) PRIMARY KEY, site VARCHAR(140) NOT NULL, enabled TINYINT(1) NOT NULL DEFAULT 1, password_hash VARCHAR(255) NOT NULL) ENGINE=InnoDB`,
			`CREATE TABLE _kora_auth_revision (id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL) ENGINE=InnoDB`,
			`CREATE TABLE _kora_config_revision (id INTEGER PRIMARY KEY, revision BIGINT NOT NULL) ENGINE=InnoDB`,
		}, mustRequestSchema(t, dialect)...) {
			if _, err := database.ExecContext(ctx, statement); err != nil {
				t.Fatalf("initialize request schema in %q: %v", databaseName, err)
			}
		}
		for _, statement := range db.OutboxTablesMySQL() {
			if _, err := database.ExecContext(ctx, statement); err != nil {
				t.Fatalf("initialize outbox schema in %q: %v", databaseName, err)
			}
		}
		siteName := fmt.Sprintf("tenant-%c.example.test", 'a'+rune(index))
		for _, user := range []string{"requester", "recipient"} {
			if _, err := database.ExecContext(ctx, `INSERT INTO _kora_user (name,site,enabled,password_hash) VALUES (?,?,1,'test')`, user, siteName); err != nil {
				t.Fatalf("seed tenant user in %q: %v", databaseName, err)
			}
		}
		if _, err := database.ExecContext(ctx, `INSERT INTO _kora_auth_revision (id,policy_revision,membership_revision) VALUES (1,1,1)`); err != nil {
			t.Fatal("seed authorization revision:", err)
		}
		if _, err := database.ExecContext(ctx, `INSERT INTO _kora_config_revision (id,revision) VALUES (1,1)`); err != nil {
			t.Fatal("seed tenant config revision:", err)
		}
		store, err := requestsql.New(database, dialect)
		if err != nil {
			t.Fatal("construct tenant request store:", err)
		}
		authorizer, err := requestsql.NewDirectTaskAuthorizer(database, dialect)
		if err != nil {
			t.Fatal("construct tenant request authorizer:", err)
		}
		service, err := requests.NewService(store, authorizer, requests.DirectTaskProjector{}, requests.NoResponseSchemas{}, apiRequestClock{}, apiRequestIDs{})
		if err != nil {
			t.Fatal("construct tenant request service:", err)
		}
		tenantID := contract.TenantID(fmt.Sprintf("tenant-isolation-%c", 'a'+rune(index)))
		execution, err := policy.NewVerifiedExecution(tenantID, contract.ActorContext{
			PrincipalID: "requester", PrincipalType: contract.PrincipalHuman, SubjectUserID: "requester", Site: siteName, AuthenticatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal("construct verified tenant execution:", err)
		}
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set("site_name", siteName)
			c.Set("site_runtime_services", SiteRuntimeService{Requests: service})
			c.Set("policy_execution", execution)
			c.Next()
		})
		RegisterRequestRoutes(router.Group("/api/v2"), nil)
		tenantRouters = append(tenantRouters, router)
	}

	command := contract.SubmitRequest{
		Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(contract.NewID())},
		Kind: contract.RequestTask, Title: "Tenant A private task", Message: "must remain tenant scoped",
		Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "recipient"}}},
	}
	body, err := json.Marshal(command)
	if err != nil {
		t.Fatal("encode request command:", err)
	}
	created := httptest.NewRecorder()
	tenantRouters[0].ServeHTTP(created, httptest.NewRequest(http.MethodPost, "/api/v2/requests", bytes.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("tenant A submit status=%d body=%s", created.Code, created.Body.String())
	}
	var createdEnvelope struct {
		Data contract.RequestView `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdEnvelope); err != nil || createdEnvelope.Data.ID == "" || len(createdEnvelope.Data.Assignments) != 1 {
		t.Fatalf("decode tenant A request: view=%+v error=%v", createdEnvelope.Data, err)
	}

	path := "/api/v2/requests/" + string(createdEnvelope.Data.ID)
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{name: "read", method: http.MethodGet, path: path},
		{name: "respond", method: http.MethodPost, path: path + "/responses", body: contract.RespondRequest{
			Meta:         contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(contract.NewID()), ExpectedRevision: createdEnvelope.Data.Revision},
			AssignmentID: createdEnvelope.Data.Assignments[0].ID, Response: contract.ResponseComplete,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requestBody *bytes.Reader
			if test.body == nil {
				requestBody = bytes.NewReader(nil)
			} else {
				encoded, err := json.Marshal(test.body)
				if err != nil {
					t.Fatal("encode cross-tenant command:", err)
				}
				requestBody = bytes.NewReader(encoded)
			}
			recorder := httptest.NewRecorder()
			tenantRouters[1].ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, requestBody))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("tenant B %s status=%d body=%s; want 404", test.name, recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), command.Title) || strings.Contains(recorder.Body.String(), string(createdEnvelope.Data.ID)) {
				t.Fatalf("tenant B %s response disclosed tenant A data: %s", test.name, recorder.Body.String())
			}
		})
	}

}
