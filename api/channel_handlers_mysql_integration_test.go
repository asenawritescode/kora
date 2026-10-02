package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

// TestLiveMySQLManagedCredentialIdempotency exercises credential retries and
// first-issue concurrency against a disposable MySQL schema.
func TestLiveMySQLManagedCredentialIdempotency(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_MYSQL_DSN to a disposable MySQL server")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse live MySQL DSN:", err)
	}
	cfg.DBName = "mysql"
	adminDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open MySQL admin connection:", err)
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatal("ping disposable MySQL server:", err)
	}
	schema := fmt.Sprintf("kora_managed_credential_%d", time.Now().UnixNano())
	if _, err := adminDB.Exec("CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create isolated MySQL schema:", err)
	}
	defer func() {
		if _, err := adminDB.Exec("DROP DATABASE `" + schema + "`"); err != nil {
			t.Errorf("drop isolated MySQL schema %s: %v", schema, err)
		}
	}()
	cfg.DBName = schema
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated MySQL schema:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(12)
	if err := database.Ping(); err != nil {
		t.Fatal("ping isolated MySQL schema:", err)
	}
	for _, statement := range kdb.ExtensibilityTablesMySQL() {
		if strings.HasPrefix(statement, "CREATE TABLE IF NOT EXISTS _kora_extension") {
			// Model a pre-upgrade tenant DB so the real additive migration below
			// proves existing customer schemas gain the new column.
			statement = strings.Replace(statement, "managed_idempotency_key CHAR(64) NOT NULL DEFAULT '',\n", "", 1)
		}
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create MySQL extension fixture: %v\nSQL: %s", err, statement)
		}
	}
	var migrationColumnCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '_kora_extension' AND column_name = 'managed_idempotency_key'`).Scan(&migrationColumnCount); err != nil {
		t.Fatal("verify MySQL idempotency migration:", err)
	}
	if migrationColumnCount != 1 {
		t.Fatal("existing MySQL extension schema was not upgraded with managed_idempotency_key")
	}
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("mysql")})
	gin.SetMode(gin.TestMode)
	invoke := func(key string) (*httptest.ResponseRecorder, error) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/channel/managed-client/rotate", strings.NewReader("{}"))
		ctx.Request.Header.Set("Idempotency-Key", key)
		ctx.Set("site_db", database)
		ctx.Set("site_db_type", "mysql")
		ctx.Set("site_name", "integration-site")
		ctx.Set("auth_type", "engine_provisioner")
		handler.HandleManagedChannelClientRotate(ctx)
		return recorder, nil
	}
	missingKey, _ := invoke("")
	if missingKey.Code != http.StatusBadRequest {
		t.Fatalf("request without Idempotency-Key returned HTTP %d, want 400", missingKey.Code)
	}

	const retryKey = "mysql-parallel-attempt-0001"
	const callers = 12
	results := make([]string, callers)
	statuses := make([]int, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recorder, _ := invoke(retryKey)
			statuses[i] = recorder.Code
			var payload struct {
				Data managedChannelClientResponse `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err == nil {
				results[i] = payload.Data.AccessToken
			}
		}(i)
	}
	close(start)
	wg.Wait()
	firstToken := ""
	for i := range callers {
		if statuses[i] != http.StatusOK || results[i] == "" {
			t.Fatalf("same-key concurrent issue %d returned HTTP %d", i, statuses[i])
		}
		if firstToken == "" {
			firstToken = results[i]
		} else if results[i] != firstToken {
			t.Fatal("same idempotency key returned different access tokens concurrently")
		}
	}
	var storedToken string
	if err := database.QueryRow(`SELECT access_token FROM _kora_extension WHERE site = ? AND name = ?`, "integration-site", "kora-cloud-channel").Scan(&storedToken); err != nil {
		t.Fatal("read persisted managed credential:", err)
	}
	if storedToken != firstToken {
		t.Fatal("persisted managed credential differs from concurrent retry response")
	}
	replay, _ := invoke(retryKey)
	if replay.Code != http.StatusOK {
		t.Fatalf("same-key replay returned HTTP %d: %s", replay.Code, replay.Body.String())
	}
	var replayPayload struct {
		Data managedChannelClientResponse `json:"data"`
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayPayload); err != nil || replayPayload.Data.AccessToken != firstToken {
		t.Fatal("same idempotency key did not replay the stored access token")
	}
	var competingResults [2]*httptest.ResponseRecorder
	var competingWG sync.WaitGroup
	competingStart := make(chan struct{})
	for i, key := range []string{"mysql-parallel-attempt-0002", "mysql-parallel-attempt-0003"} {
		competingWG.Add(1)
		go func(i int, key string) {
			defer competingWG.Done()
			<-competingStart
			competingResults[i], _ = invoke(key)
		}(i, key)
	}
	close(competingStart)
	competingWG.Wait()
	rotationWinner := ""
	rotationSuccesses, rotationConflicts := 0, 0
	for _, response := range competingResults {
		switch response.Code {
		case http.StatusOK:
			rotationSuccesses++
			var payload struct {
				Data managedChannelClientResponse `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal("decode winning rotation response:", err)
			}
			rotationWinner = payload.Data.AccessToken
		case http.StatusConflict:
			rotationConflicts++
		default:
			t.Fatalf("competing rotation returned HTTP %d: %s", response.Code, response.Body.String())
		}
	}
	if rotationSuccesses != 1 || rotationConflicts != 1 {
		t.Fatalf("competing rotations: successes=%d conflicts=%d, want one each", rotationSuccesses, rotationConflicts)
	}
	if err := database.QueryRow(`SELECT access_token FROM _kora_extension WHERE site = ? AND name = ?`, "integration-site", "kora-cloud-channel").Scan(&storedToken); err != nil {
		t.Fatal("read rotated managed credential:", err)
	}
	if storedToken == firstToken || storedToken != rotationWinner {
		t.Fatal("persisted credential does not match the winning competing rotation")
	}
}
