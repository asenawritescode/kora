package api

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAuthProviderRoutesRequireAdministrator(t *testing.T) {
	r, _ := adminGuardRouter(t, []string{"Sales User"}, registerManagementRoutes)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/system/auth/providers"},
		{http.MethodPut, "/api/system/auth/providers/kora-cloud"},
		{http.MethodDelete, "/api/system/auth/providers/kora-cloud"},
		{http.MethodGet, "/api/system/auth/policy"},
		{http.MethodPut, "/api/system/auth/policy"},
	} {
		if w := serve(r, tc.method, tc.path, `{}`); w.Code != http.StatusForbidden {
			t.Fatalf("%s %s status = %d, want 403", tc.method, tc.path, w.Code)
		}
	}
}

func TestAuthProviderPutValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		mock     func(sqlmock.Sqlmock)
		wantCode int
		wantText string
	}{
		{
			name:     "plain http issuer rejected",
			body:     `{"issuer":"http://idp.example.com","client_id":"studio"}`,
			wantCode: http.StatusBadRequest,
			wantText: "validation.invalid_provider",
		},
		{
			name: "enabling without a client secret rejected",
			body: `{"issuer":"https://idp.example.com","client_id":"studio","enabled":true}`,
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery("_kora_secret").WillReturnError(sql.ErrNoRows)
			},
			wantCode: http.StatusBadRequest,
			wantText: "validation.missing_client_secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := adminGuardRouter(t, []string{"Administrator"}, registerManagementRoutes)
			if tc.mock != nil {
				tc.mock(mock)
			}
			w := serve(r, http.MethodPut, "/api/system/auth/providers/kora-cloud", tc.body)
			if w.Code != tc.wantCode || !strings.Contains(w.Body.String(), tc.wantText) {
				t.Fatalf("status = %d body = %s, want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantText)
			}
		})
	}
}

func TestAuthPolicyRefusesLockout(t *testing.T) {
	r, mock := adminGuardRouter(t, []string{"Administrator"}, registerManagementRoutes)
	mock.ExpectQuery("FROM _kora_auth_provider").WillReturnRows(sqlmock.NewRows([]string{"id", "display_name", "issuer", "client_id", "client_secret_key", "scopes", "enabled"}))
	w := serve(r, http.MethodPut, "/api/system/auth/policy", `{"allow_local_sign_in":false}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "auth.last_sign_in_method") {
		t.Fatalf("status = %d body = %s, want 409 auth.last_sign_in_method", w.Code, w.Body.String())
	}
}
