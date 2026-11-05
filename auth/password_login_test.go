package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

const passwordLoginSelect = `SELECT name, email, full_name, enabled, COALESCE\(roles, ''\), password_hash, email_verified_at FROM _kora_user`

func passwordLoginRouter(t *testing.T, site string, disabled bool) (*gin.Engine, sqlmock.Sqlmock) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	t.Cleanup(func() { db.Close() })
	sm := NewSessionManager(db)
	t.Cleanup(func() { sm.Close() })
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", site)
		c.Set("site_db", db)
		c.Set("site_db_type", "mysql")
		if disabled {
			c.Set(PasswordSignInDisabledContextKey, true)
		}
	})
	RegisterAuthRoutes(router, sm, db, nil)
	return router, mock
}

func postPasswordLogin(router *gin.Engine, body string, csrf bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if csrf {
		request.AddCookie(&http.Cookie{Name: "kora_csrf", Value: "csrf-token"})
		request.Header.Set("X-Kora-CSRF-Token", "csrf-token")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return body.Error.Code
}

func TestPasswordLogin(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	userRow := func(enabled bool, passwordHash string, verified bool) *sqlmock.Rows {
		var verifiedAt any
		if verified {
			verifiedAt = time.Now()
		}
		return sqlmock.NewRows([]string{"name", "email", "full_name", "enabled", "roles", "password_hash", "email_verified_at"}).
			AddRow("USER-1", "amina@example.co.ke", "Amina Hassan", enabled, "Sales User", passwordHash, verifiedAt)
	}
	for _, tc := range []struct {
		name       string
		body       string
		csrf       bool
		disabled   bool
		expect     func(sqlmock.Sqlmock, string)
		wantStatus int
		wantCode   string
	}{
		{
			name: "valid password signs in", body: `{"email":"Amina@example.co.ke","password":"correct horse"}`, csrf: true,
			expect: func(m sqlmock.Sqlmock, site string) {
				m.ExpectQuery(passwordLoginSelect).WithArgs(site, "Amina@example.co.ke").WillReturnRows(userRow(true, string(hash), true))
				m.ExpectExec("INSERT INTO _kora_session").WillReturnResult(sqlmock.NewResult(1, 1))
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "wrong password", body: `{"email":"amina@example.co.ke","password":"wrong"}`, csrf: true,
			expect: func(m sqlmock.Sqlmock, site string) {
				m.ExpectQuery(passwordLoginSelect).WithArgs(site, "amina@example.co.ke").WillReturnRows(userRow(true, string(hash), true))
			},
			wantStatus: http.StatusUnauthorized, wantCode: "auth.invalid_credentials",
		},
		{
			name: "unknown email gets the same answer", body: `{"email":"nobody@example.co.ke","password":"correct horse"}`, csrf: true,
			expect: func(m sqlmock.Sqlmock, site string) {
				m.ExpectQuery(passwordLoginSelect).WithArgs(site, "nobody@example.co.ke").WillReturnRows(sqlmock.NewRows([]string{"name"}))
			},
			wantStatus: http.StatusUnauthorized, wantCode: "auth.invalid_credentials",
		},
		{
			name: "disabled account is refused", body: `{"email":"amina@example.co.ke","password":"correct horse"}`, csrf: true,
			expect: func(m sqlmock.Sqlmock, site string) {
				m.ExpectQuery(passwordLoginSelect).WithArgs(site, "amina@example.co.ke").WillReturnRows(userRow(false, string(hash), true))
			},
			wantStatus: http.StatusUnauthorized, wantCode: "auth.invalid_credentials",
		},
		{
			name: "passwordless account never matches", body: `{"email":"amina@example.co.ke","password":"$kora$passwordless$disabled"}`, csrf: true,
			expect: func(m sqlmock.Sqlmock, site string) {
				m.ExpectQuery(passwordLoginSelect).WithArgs(site, "amina@example.co.ke").WillReturnRows(userRow(true, "$kora$passwordless$disabled", true))
			},
			wantStatus: http.StatusUnauthorized, wantCode: "auth.invalid_credentials",
		},
		{
			name: "unverified email must confirm first", body: `{"email":"amina@example.co.ke","password":"correct horse"}`, csrf: true,
			expect: func(m sqlmock.Sqlmock, site string) {
				m.ExpectQuery(passwordLoginSelect).WithArgs(site, "amina@example.co.ke").WillReturnRows(userRow(true, string(hash), false))
			},
			wantStatus: http.StatusForbidden, wantCode: "auth.email_verification_required",
		},
		{
			name: "missing CSRF token", body: `{"email":"amina@example.co.ke","password":"correct horse"}`,
			wantStatus: http.StatusForbidden, wantCode: "csrf.token_required",
		},
		{
			name: "single sign-on only workspace", body: `{"email":"amina@example.co.ke","password":"correct horse"}`, csrf: true, disabled: true,
			wantStatus: http.StatusForbidden, wantCode: "auth.password_sign_in_disabled",
		},
		{
			name: "missing password", body: `{"email":"amina@example.co.ke"}`, csrf: true,
			wantStatus: http.StatusBadRequest, wantCode: "auth.invalid_request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := "pw-" + strings.ReplaceAll(tc.name, " ", "-")
			router, mock := passwordLoginRouter(t, site, tc.disabled)
			if tc.expect != nil {
				tc.expect(mock, site)
			}
			response := postPasswordLogin(router, tc.body, tc.csrf)
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
			if tc.wantCode != "" {
				if code := errorCode(t, response); code != tc.wantCode {
					t.Fatalf("code = %q, want %q", code, tc.wantCode)
				}
			}
			setsSession := strings.Contains(strings.Join(response.Header().Values("Set-Cookie"), ";"), "kora_sid=")
			if setsSession != (tc.wantStatus == http.StatusOK) {
				t.Fatalf("session cookie set = %v for status %d", setsSession, response.Code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPasswordLoginRateLimitsRepeatedAttempts(t *testing.T) {
	site := "pw-rate-limit"
	router, mock := passwordLoginRouter(t, site, false)
	for i := 0; i < authRateLimitMax; i++ {
		mock.ExpectQuery(passwordLoginSelect).WithArgs(site, "amina@example.co.ke").WillReturnRows(sqlmock.NewRows([]string{"name"}))
		if response := postPasswordLogin(router, `{"email":"amina@example.co.ke","password":"guess"}`, true); response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", i+1, response.Code)
		}
	}
	response := postPasswordLogin(router, `{"email":"amina@example.co.ke","password":"guess"}`, true)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
