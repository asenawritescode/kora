package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/auth"
	"github.com/gin-gonic/gin"
)

func TestHandleManagedChannelClientRotateStoresOnlyVerifier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_ENGINE_PROVISIONING_TOKEN", "provisioning-secret")
	const key = "rotate-op-123456"
	token, err := managedChannelToken("tenant-1", key)
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := auth.HashExtensionAccessToken(token)
	selectCurrent := "SELECT access_token_hash, managed_idempotency_key FROM _kora_extension WHERE site = \\? AND name = \\?"

	t.Run("first rotation inserts the verifier and returns the token once", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(selectCurrent).WithArgs("tenant-1", "kora-cloud-channel").WillReturnError(sql.ErrNoRows)
		mock.ExpectBegin()
		mock.ExpectExec("INSERT INTO _kora_extension .* VALUES \\(\\?, \\?, \\?, \\?, \\?, \\?, '', \\?").
			WithArgs("kora-cloud-channel", "tenant-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), tokenHash, sqlmock.AnyArg(), "[]", "[]").
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectCommit()
		response := invokeManagedCredentialRotate(t, db, key)
		if response.Code != http.StatusOK || rotatedToken(t, response) != token {
			t.Fatalf("unexpected rotation response %d: %s", response.Code, response.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replay with the same key returns the same token without writing", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery(selectCurrent).WithArgs("tenant-1", "kora-cloud-channel").
			WillReturnRows(sqlmock.NewRows([]string{"access_token_hash", "managed_idempotency_key"}).AddRow(tokenHash, auth.HashExtensionAccessToken(key)))
		response := invokeManagedCredentialRotate(t, db, key)
		if response.Code != http.StatusOK || rotatedToken(t, response) != token {
			t.Fatalf("unexpected replay response %d: %s", response.Code, response.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replay after the provisioning secret changed is refused", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		t.Setenv("KORA_ENGINE_PROVISIONING_TOKEN", "rotated-provisioning-secret")
		mock.ExpectQuery(selectCurrent).WithArgs("tenant-1", "kora-cloud-channel").
			WillReturnRows(sqlmock.NewRows([]string{"access_token_hash", "managed_idempotency_key"}).AddRow(tokenHash, auth.HashExtensionAccessToken(key)))
		response := invokeManagedCredentialRotate(t, db, key)
		if response.Code != http.StatusConflict {
			t.Fatalf("stale replay returned %d: %s", response.Code, response.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func invokeManagedCredentialRotate(t *testing.T, db *sql.DB, key string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/channel/managed-client/rotate", nil)
	ctx.Request.Header.Set("Idempotency-Key", key)
	ctx.Set("site_db", db)
	ctx.Set("site_db_type", "mysql")
	ctx.Set("site_name", "tenant-1")
	ctx.Set("auth_type", "engine_provisioner")
	NewHandler(nil, nil).HandleManagedChannelClientRotate(ctx)
	return recorder
}

func rotatedToken(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode rotation response: %v", err)
	}
	return body.Data.AccessToken
}
