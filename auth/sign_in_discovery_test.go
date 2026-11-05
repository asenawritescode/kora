package auth

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func signInRouter(t *testing.T, mailer MagicLinkMailer) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sm := NewSessionManager(db)
	t.Cleanup(func() { _ = sm.Close() })
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_db", db)
		c.Set("site_name", "s1")
		c.Set("site_db_type", "mysql")
	})
	if err := RegisterAuthRoutesWithPublicAppBaseURL(router, sm, db, mailer, "https://app.kora.test"); err != nil {
		t.Fatal(err)
	}
	return router, mock
}

func TestSignInDiscoveryListsOnlyEnabledProviders(t *testing.T) {
	router, mock := signInRouter(t, testMagicLinkMailer{})
	mock.ExpectQuery("FROM _kora_site_setting").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("FROM _kora_auth_provider").WillReturnRows(
		sqlmock.NewRows([]string{"id", "display_name", "issuer", "client_id", "client_secret_key", "scopes", "enabled"}).
			AddRow("kora-cloud", "Kora Cloud", "https://id.kora.test", "studio", "k", "openid email", true).
			AddRow("google", "Google", "https://accounts.google.com", "c", "k", "openid", false))

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/auth/providers", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	var body struct {
		Data signInDiscovery `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Data.MagicLink || len(body.Data.Providers) != 1 || body.Data.Providers[0].ID != "kora-cloud" || body.Data.Providers[0].StartURL != "/api/auth/oidc/kora-cloud/start" {
		t.Fatalf("discovery = %+v", body.Data)
	}
	if strings.Contains(response.Body.String(), "client") || strings.Contains(response.Body.String(), "issuer") {
		t.Fatalf("discovery leaked provider configuration: %s", response.Body.String())
	}
}

func TestMagicLinkRefusedWhenLocalSignInDisabled(t *testing.T) {
	for _, path := range []string{"/api/auth/magic-link/request", "/api/auth/magic-link/verify"} {
		t.Run(path, func(t *testing.T) {
			router, mock := signInRouter(t, testMagicLinkMailer{})
			mock.ExpectQuery("FROM _kora_site_setting").WillReturnRows(sqlmock.NewRows([]string{"setting_value"}).AddRow("false"))
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"email":"a@example.com","token":"t"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer native-api-client")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "auth.local_sign_in_disabled") {
				t.Fatalf("status = %d body = %s, want 403 auth.local_sign_in_disabled", response.Code, response.Body.String())
			}
		})
	}
}
