package auth

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestCloudHandoffRedeemsAndSetsHttpOnlySessionCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-service-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["tenant_id"] != "tenant-1" || request["audience"] != "studio" || request["code"] != "opaque-code" {
			t.Errorf("Cloud redemption request = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"usr_stable","tenant_id":"tenant-1","audience":"studio"}`))
	}))
	defer cloud.Close()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT name,email,full_name,enabled,COALESCE\(roles,''\) FROM _kora_user WHERE site=\? AND name=\?`).
		WithArgs("tenant-host", "usr_stable").WillReturnRows(sqlmock.NewRows([]string{"name", "email", "full_name", "enabled", "roles"}).AddRow("usr_stable", "a@example.test", "A User", true, "Administrator"))
	mock.ExpectExec(`INSERT INTO _kora_session`).WillReturnResult(sqlmock.NewResult(1, 1))

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("site_id", "tenant-1"); c.Set("site_name", "tenant-host"); c.Next() })
	if err := RegisterCloudHandoffRoute(router, NewSessionManager(db), db, CloudHandoffConfig{RedeemURL: cloud.URL, ServiceToken: "private-service-key"}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/cloud/handoff", strings.NewReader(`{"code":"opaque-code"}`))
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "sid") || strings.Contains(recorder.Body.String(), "session_id") {
		t.Fatalf("session credential leaked in JSON: %s", recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("cookies = %#v", cookies)
	}
	var cookie *http.Cookie
	var csrf *http.Cookie
	for _, candidate := range cookies {
		switch candidate.Name {
		case "kora_sid":
			cookie = candidate
		case "kora_csrf":
			csrf = candidate
		}
	}
	if cookie == nil {
		t.Fatalf("session cookie missing: %#v", cookies)
	}
	if cookie.Name != "kora_sid" || cookie.Value == "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %#v", cookie)
	}
	if csrf == nil || csrf.Value == "" || csrf.HttpOnly || csrf.Path != "/" || csrf.SameSite != http.SameSiteLaxMode {
		t.Fatalf("CSRF cookie = %#v", csrf)
	}
	if cookie.Expires.Before(time.Now()) {
		t.Fatalf("session cookie expires in past: %v", cookie.Expires)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudHandoffRejectsCloudTenantMismatchWithoutSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"usr_stable","tenant_id":"different-tenant","audience":"studio"}`))
	}))
	defer cloud.Close()
	var db *sql.DB
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("site_id", "tenant-1"); c.Set("site_name", "tenant-host"); c.Next() })
	if err := RegisterCloudHandoffRoute(router, NewSessionManager(nil), db, CloudHandoffConfig{RedeemURL: cloud.URL, ServiceToken: "token"}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/cloud/handoff", strings.NewReader(`{"code":"opaque-code"}`)))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Set-Cookie") != "" {
		t.Fatalf("unexpected session cookie %q", recorder.Header().Get("Set-Cookie"))
	}
}
