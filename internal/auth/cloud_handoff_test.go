package auth

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"log/slog"
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

func captureHandoffLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestCloudHandoffConfigurationVisibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secretURL = "http://cloud.secret-host.invalid/redeem"
	const secretToken = "super-secret-service-token"
	tests := []struct {
		name       string
		cfg        CloudHandoffConfig
		wantErr    string
		wantStatus int
		wantCode   string
		wantWarn   bool
		wantAbsent []string
	}{
		{name: "unset", cfg: CloudHandoffConfig{}, wantStatus: http.StatusServiceUnavailable, wantCode: "auth.cloud_handoff_unconfigured", wantWarn: true},
		{name: "blank values", cfg: CloudHandoffConfig{RedeemURL: "  ", ServiceToken: "\t"}, wantStatus: http.StatusServiceUnavailable, wantCode: "auth.cloud_handoff_unconfigured", wantWarn: true},
		{name: "only url", cfg: CloudHandoffConfig{RedeemURL: secretURL}, wantErr: "KORA_ENGINE_CLOUD_IDENTITY_TOKEN", wantAbsent: []string{secretURL, "secret-host"}},
		{name: "only token", cfg: CloudHandoffConfig{ServiceToken: secretToken}, wantErr: "KORA_ENGINE_CLOUD_IDENTITY_URL", wantAbsent: []string{secretToken}},
		{name: "fully set", cfg: CloudHandoffConfig{RedeemURL: secretURL, ServiceToken: secretToken}, wantStatus: http.StatusBadRequest, wantAbsent: []string{secretURL, secretToken}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureHandoffLogs(t)
			router := gin.New()
			err := RegisterCloudHandoffRoute(router, NewSessionManager(nil), nil, tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want mention of %s", err, tt.wantErr)
				}
				for _, secret := range tt.wantAbsent {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("error leaks configuration value %q: %v", secret, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/cloud/handoff", strings.NewReader(`{}`)))
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantCode != "" {
				var body struct {
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Code != tt.wantCode || body.Error.Message == "" || strings.Contains(body.Error.Message, "KORA_ENGINE") {
					t.Fatalf("error body = %s", recorder.Body.String())
				}
			}
			output := logs.String()
			if got := strings.Count(output, "level=WARN"); tt.wantWarn && got != 1 || !tt.wantWarn && got != 0 {
				t.Fatalf("WARN lines = %d, want warn=%v: %s", got, tt.wantWarn, output)
			}
			if tt.wantWarn {
				for _, name := range []string{"KORA_ENGINE_CLOUD_IDENTITY_URL", "KORA_ENGINE_CLOUD_IDENTITY_TOKEN"} {
					if !strings.Contains(output, name) {
						t.Fatalf("warning does not name %s: %s", name, output)
					}
				}
			}
			for _, secret := range tt.wantAbsent {
				if strings.Contains(output, secret) || strings.Contains(recorder.Body.String(), secret) {
					t.Fatalf("output leaks configuration value %q", secret)
				}
			}
		})
	}
}
