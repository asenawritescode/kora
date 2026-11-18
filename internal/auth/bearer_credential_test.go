package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBearerTokenParsesOnlyBearerCredentials(t *testing.T) {
	tests := []struct {
		header string
		token  string
		ok     bool
	}{
		{header: "Bearer abc", token: "abc", ok: true},
		{header: "bearer abc", token: "abc", ok: true},
		{header: "BEARER  abc ", token: "abc", ok: true},
		{header: "Bearer ", ok: false},
		{header: "Basic dXNlcjpwYXNz", ok: false},
		{header: "Bearerabc", ok: false},
		{header: "", ok: false},
	}
	for _, test := range tests {
		token, ok := bearerToken(test.header)
		if token != test.token || ok != test.ok {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", test.header, token, ok, test.token, test.ok)
		}
	}
}

func TestSessionIDNeverFallsBackToCookieWhenAuthorizationIsPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		header string
		cookie string
		want   string
	}{
		{name: "bearer token wins over cookie", header: "Bearer sid-header", cookie: "sid-cookie", want: "sid-header"},
		{name: "non-bearer header does not use cookie", header: "Basic dXNlcjpwYXNz", cookie: "sid-cookie", want: ""},
		{name: "empty bearer does not use cookie", header: "Bearer ", cookie: "sid-cookie", want: ""},
		{name: "cookie is used without Authorization", cookie: "sid-cookie", want: "sid-cookie"},
		{name: "no credential", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			if test.cookie != "" {
				request.AddCookie(&http.Cookie{Name: "kora_sid", Value: test.cookie})
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = request
			if got := sessionIDFromRequest(c); got != test.want {
				t.Errorf("sessionIDFromRequest = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCSRFSkipsOnlyForBearerCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		header     string
		wantPassed bool
	}{
		{name: "bearer skips CSRF", header: "Bearer token", wantPassed: true},
		{name: "lowercase bearer scheme skips CSRF", header: "bearer token", wantPassed: true},
		{name: "non-bearer authorization does not skip CSRF", header: "Basic dXNlcjpwYXNz"},
		{name: "no authorization requires CSRF token", wantPassed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			passed := false
			router.Use(CSRFMiddleware())
			router.POST("/x", func(c *gin.Context) { passed = true; c.Status(http.StatusOK) })
			request := httptest.NewRequest(http.MethodPost, "/x", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			router.ServeHTTP(httptest.NewRecorder(), request)
			if passed != test.wantPassed {
				t.Errorf("handler reached = %v, want %v", passed, test.wantPassed)
			}
		})
	}
}

func TestSiteGuardRefusesNonBearerAuthorizationWithCookieSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guard := &SiteGuard{}
	router := gin.New()
	reached := false
	router.Use(guard.Middleware(false))
	router.GET("/api/v1/x", func(c *gin.Context) { reached = true; c.Status(http.StatusOK) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	request.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	request.AddCookie(&http.Cookie{Name: "kora_sid", Value: "sid-cookie"})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || reached {
		t.Fatalf("non-Bearer Authorization with a cookie: status %d reached=%v, want 401", recorder.Code, reached)
	}
}
