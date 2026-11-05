package api

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/auth"
	"github.com/gin-gonic/gin"
)

type capturedArg struct{ value *string }

func (c capturedArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	*c.value = s
	return ok
}

func TestHandleExtensionCreateStoresOnlyTokenVerifier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var storedHash string
	mock.ExpectExec("INSERT INTO _kora_extension \\(name, site, display_name, description, endpoint_url, secret, access_token, access_token_hash, .*VALUES \\(\\?, \\?, \\?, \\?, \\?, \\?, '', \\?").
		WithArgs("billing-bot", "tenant-1", "Billing", "", "https://203.0.113.10/kora", sqlmock.AnyArg(), capturedArg{&storedHash}, sqlmock.AnyArg(), "[]").
		WillReturnResult(sqlmock.NewResult(1, 1))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/extension", strings.NewReader(`{"name":"billing-bot","display_name":"Billing","endpoint_url":"https://203.0.113.10/kora"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_db", db)
	ctx.Set("site_name", "tenant-1")
	NewHandler(nil, nil).HandleExtensionCreate(ctx)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Data extensionCreatedResponse `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.AccessToken == "" || storedHash != auth.HashExtensionAccessToken(body.Data.AccessToken) {
		t.Fatalf("stored verifier %q does not match the token returned once", storedHash)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
