package api

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/gin-gonic/gin"
)

func TestHandleManagedChannelClientRevokeIsIdempotentAndFingerprintBound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const token = "current-managed-token"
	fingerprint := sha256.Sum256([]byte(token))
	body, _ := json.Marshal(map[string]string{"credential_fingerprint": hex.EncodeToString(fingerprint[:])})
	tokenHash := auth.HashExtensionAccessToken(token)

	t.Run("revokes the expected active token", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery("SELECT access_token_hash FROM _kora_extension WHERE site = \\? AND name = \\?").WithArgs("tenant-1", "kora-cloud-channel").WillReturnRows(sqlmock.NewRows([]string{"access_token_hash"}).AddRow(tokenHash))
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE _kora_extension SET is_active = FALSE, managed_idempotency_key = \\?, updated_at = CURRENT_TIMESTAMP WHERE site = \\? AND name = \\? AND access_token_hash = \\? AND is_active = TRUE").WithArgs(sqlmock.AnyArg(), "tenant-1", "kora-cloud-channel", tokenHash).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("UPDATE _kora_channel_session SET revoked_at = CURRENT_TIMESTAMP, revoked_reason = \\? WHERE site = \\? AND client_name = \\? AND revoked_at IS NULL").WithArgs("tenant_deletion", "tenant-1", "kora-cloud-channel").WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectCommit()
		response := invokeManagedCredentialRevoke(t, NewHandler(nil, nil), db, body, true)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revoked":true`) || strings.Contains(response.Body.String(), token) {
			t.Fatalf("unexpected revocation response %d: %s", response.Code, response.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("accepts a repeated revocation", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery("SELECT access_token_hash FROM _kora_extension WHERE site = \\? AND name = \\?").WithArgs("tenant-1", "kora-cloud-channel").WillReturnRows(sqlmock.NewRows([]string{"access_token_hash"}).AddRow(tokenHash))
		mock.ExpectBegin()
		mock.ExpectExec("UPDATE _kora_extension SET is_active = FALSE, managed_idempotency_key = \\?, updated_at = CURRENT_TIMESTAMP WHERE site = \\? AND name = \\? AND access_token_hash = \\? AND is_active = TRUE").WithArgs(sqlmock.AnyArg(), "tenant-1", "kora-cloud-channel", tokenHash).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectRollback()
		mock.ExpectQuery("SELECT access_token_hash, is_active FROM _kora_extension WHERE site = \\? AND name = \\?").WithArgs("tenant-1", "kora-cloud-channel").WillReturnRows(sqlmock.NewRows([]string{"access_token_hash", "is_active"}).AddRow(tokenHash, false))
		mock.ExpectExec("UPDATE _kora_channel_session SET revoked_at = CURRENT_TIMESTAMP, revoked_reason = \\? WHERE site = \\? AND client_name = \\? AND revoked_at IS NULL").WithArgs("tenant_deletion", "tenant-1", "kora-cloud-channel").WillReturnResult(sqlmock.NewResult(0, 0))
		response := invokeManagedCredentialRevoke(t, NewHandler(nil, nil), db, body, true)
		if response.Code != http.StatusOK {
			t.Fatalf("repeated revocation returned %d: %s", response.Code, response.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("does not revoke a newer token", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mock.ExpectQuery("SELECT access_token_hash FROM _kora_extension WHERE site = \\? AND name = \\?").WithArgs("tenant-1", "kora-cloud-channel").WillReturnRows(sqlmock.NewRows([]string{"access_token_hash"}).AddRow(auth.HashExtensionAccessToken("newer-managed-token")))
		response := invokeManagedCredentialRevoke(t, NewHandler(nil, nil), db, body, true)
		if response.Code != http.StatusConflict {
			t.Fatalf("stale revocation returned %d: %s", response.Code, response.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHandleManagedChannelClientRevokeRequiresProvisionerAndIdempotencyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name        string
		provisioner bool
		key         string
		want        int
	}{
		{name: "requires provisioner", key: "delete-op-123456", want: http.StatusForbidden},
		{name: "requires operation key", provisioner: true, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := invokeManagedCredentialRevoke(t, NewHandler(nil, nil), nil, []byte(`{"credential_fingerprint":"`+strings.Repeat("a", 64)+`"}`), tc.provisioner, tc.key)
			if response.Code != tc.want {
				t.Fatalf("returned %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}

func TestHandleManagedChannelClientRotateRejectsSuspendedTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/channel/managed-client/rotate", strings.NewReader(`{}`))
	ctx.Request.Header.Set("Idempotency-Key", "rotate-during-delete-0001")
	ctx.Set("auth_type", "engine_provisioner")
	ctx.Set("site_status", "suspended")
	NewHandler(nil, nil).HandleManagedChannelClientRotate(ctx)
	if response.Code != http.StatusConflict {
		t.Fatalf("credential rotation for suspended tenant returned %d, want 409: %s", response.Code, response.Body.String())
	}
}

func invokeManagedCredentialRevoke(t *testing.T, handler *Handler, db *sql.DB, body []byte, provisioner bool, key ...string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/internal/channel/managed-client/revoke", strings.NewReader(string(body)))
	ctx.Request.Header.Set("Idempotency-Key", "delete-op-123456")
	if len(key) > 0 {
		ctx.Request.Header.Set("Idempotency-Key", key[0])
	}
	ctx.Set("site_db", db)
	ctx.Set("site_db_type", "mysql")
	ctx.Set("site_name", "tenant-1")
	if provisioner {
		ctx.Set("auth_type", "engine_provisioner")
	}
	handler.HandleManagedChannelClientRevoke(ctx)
	return recorder
}
