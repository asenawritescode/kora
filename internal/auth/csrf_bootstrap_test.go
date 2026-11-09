package auth

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// TestCleanBrowserSignInBootstrapsCSRF drives the sign-in routes the way a
// browser with an empty cookie jar does: the only CSRF value it can send is
// the one the Engine handed out on an earlier unauthenticated GET.
func TestCleanBrowserSignInBootstrapsCSRF(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		prefix    string // path-routing prefix stripped before the Engine routes see the request
		bootstrap string
	}{
		{name: "host routing via providers", bootstrap: "/api/auth/providers"},
		{name: "host routing via me", bootstrap: "/api/auth/me"},
		{name: "path routing via providers", prefix: "/s/csrf-boot", bootstrap: "/api/auth/providers"},
		{name: "path routing via me", prefix: "/s/csrf-boot", bootstrap: "/api/auth/me"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := "csrf-" + strings.ReplaceAll(tc.name, " ", "-")
			gin.SetMode(gin.TestMode)
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			mock.MatchExpectationsInOrder(false)
			sm := NewSessionManager(db)
			t.Cleanup(func() { _ = sm.Close() })
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set("site_db", db)
				c.Set("site_name", site)
				c.Set("site_db_type", "mysql")
			})
			if err := RegisterAuthRoutesWithPublicAppBaseURL(router, sm, db, testMagicLinkMailer{}, "https://app.kora.test"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				expectLocalSignInPolicy(mock, site, "")
			}
			mock.ExpectQuery("FROM _kora_auth_provider").WillReturnRows(sqlmock.NewRows([]string{"id"}))
			mock.ExpectQuery(passwordLoginSelect).WithArgs(site, "amina@example.co.ke").WillReturnRows(
				sqlmock.NewRows([]string{"name", "email", "full_name", "enabled", "roles", "password_hash", "email_verified_at"}).
					AddRow("USER-1", "amina@example.co.ke", "Amina Hassan", true, "Sales User", string(hash), time.Now()))
			mock.ExpectExec("INSERT INTO _kora_session").WillReturnResult(sqlmock.NewResult(1, 1))

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, tc.prefix)
				router.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Jar: jar}
			base, _ := url.Parse(server.URL)

			// Nothing in the jar: a state-changing request must still be refused.
			if status, body := post(t, client, server.URL+tc.prefix+"/api/auth/magic-link/request", `{"email":"amina@example.co.ke"}`, ""); status != http.StatusForbidden || !strings.Contains(body, "csrf.token_required") {
				t.Fatalf("cookie-less POST = %d %s, want 403 csrf.token_required", status, body)
			}

			resp, err := client.Get(server.URL + tc.prefix + tc.bootstrap)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			token := ""
			for _, cookie := range jar.Cookies(base) {
				if cookie.Name == "kora_csrf" {
					token = cookie.Value
				}
			}
			if token == "" {
				t.Fatalf("GET %s (status %d) did not issue kora_csrf", tc.bootstrap, resp.StatusCode)
			}

			// The cookie alone is not enough: a header-less POST stays refused.
			if status, body := post(t, client, server.URL+tc.prefix+"/api/auth/login", `{"email":"amina@example.co.ke","password":"correct horse"}`, ""); status != http.StatusForbidden || !strings.Contains(body, "csrf.token_mismatch") {
				t.Fatalf("header-less POST = %d %s, want 403 csrf.token_mismatch", status, body)
			}

			for _, step := range []struct {
				path, body string
				want       int
			}{
				{"/api/auth/magic-link/request", `{"email":"amina@example.co.ke"}`, http.StatusAccepted},
				{"/api/auth/magic-link/verify", `{"token":"not-a-real-token"}`, http.StatusUnauthorized},
				{"/api/auth/login", `{"email":"amina@example.co.ke","password":"correct horse"}`, http.StatusOK},
			} {
				status, body := post(t, client, server.URL+tc.prefix+step.path, step.body, token)
				if status != step.want {
					t.Fatalf("POST %s = %d %s, want %d", step.path, status, body, step.want)
				}
			}
		})
	}
}

func post(t *testing.T, client *http.Client, target, body, csrf string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		request.Header.Set("X-Kora-CSRF-Token", csrf)
	}
	resp, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}
