package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/auth/oidc"
	"github.com/gin-gonic/gin"
)

type fakeProviders map[string]OIDCProvider

func (f fakeProviders) Get(_ context.Context, _ string, id string) (OIDCProvider, error) {
	p, ok := f[id]
	if !ok {
		return OIDCProvider{}, ErrProviderNotFound
	}
	return p, nil
}

type fakeSecrets map[string]string

func (f fakeSecrets) Get(_ string, key string) (string, error) {
	v, ok := f[key]
	if !ok {
		return "", errors.New("secret not found")
	}
	return v, nil
}

type fakeStates struct {
	mu     sync.Mutex
	states map[string]oidcLoginState
}

func (f *fakeStates) Save(_ context.Context, hash string, st oidcLoginState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[string]oidcLoginState{}
	}
	f.states[hash] = st
	return nil
}

func (f *fakeStates) Consume(_ context.Context, site, hash string, now time.Time) (oidcLoginState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[hash]
	if !ok {
		return oidcLoginState{}, oidc.ErrStateInvalid
	}
	delete(f.states, hash)
	if st.Site != site {
		return oidcLoginState{}, oidc.ErrStateSiteMismatch
	}
	if !now.Before(st.ExpiresAt) {
		return oidcLoginState{}, oidc.ErrStateExpired
	}
	return st, nil
}

func (f *fakeStates) only(t *testing.T) oidcLoginState {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.states) != 1 {
		t.Fatalf("saved states = %d, want 1", len(f.states))
	}
	for _, st := range f.states {
		return st
	}
	return oidcLoginState{}
}

// fakeIssuer serves an OIDC discovery document for its own URL.
func fakeIssuer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oidcMetadata{
			Issuer: server.URL, AuthorizationEndpoint: server.URL + "/authorize",
			TokenEndpoint: server.URL + "/token", JWKSURI: server.URL + "/jwks",
		})
	})
	return server
}

func oidcRouter(login *OIDCLogin) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("site_name", "s1") })
	login.Register(router.Group("/api/auth"))
	return router
}

func TestOIDCStartRedirectsWithPKCEAndBindsBrowser(t *testing.T) {
	issuer := fakeIssuer(t, http.NewServeMux())
	states := &fakeStates{}
	login := &OIDCLogin{HTTPClient: issuer.Client(), PublicAppBaseURL: "https://app.kora.test", Deps: func(*gin.Context) (oidcLoginDeps, error) {
		return oidcLoginDeps{Providers: fakeProviders{"kora-cloud": OIDCProvider{ID: "kora-cloud", Issuer: issuer.URL, ClientID: "studio", Enabled: true}.Normalize()}, States: states}, nil
	}}
	response := httptest.NewRecorder()
	oidcRouter(login).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/kora-cloud/start?return_to=/workspace/Sales%20Order", nil))
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(location.String(), issuer.URL+"/authorize?") {
		t.Fatalf("redirect = %q", response.Header().Get("Location"))
	}
	q := location.Query()
	pending := states.only(t)
	if q.Get("client_id") != "studio" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != oidc.S256Challenge(pending.CodeVerifier) ||
		q.Get("nonce") != pending.Nonce || q.Get("redirect_uri") != "https://app.kora.test/api/auth/oidc/kora-cloud/callback" || !strings.HasPrefix(q.Get("scope"), "openid") {
		t.Fatalf("authorize query = %v, pending = %+v", q, pending)
	}
	if pending.ReturnTo != "/workspace/Sales Order" || pending.ProviderID != "kora-cloud" || pending.Site != "s1" {
		t.Fatalf("pending = %+v", pending)
	}
	cookie := response.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, oidcBindingCookie+"=") || !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "Path=/api/auth/oidc") {
		t.Fatalf("binding cookie = %q", cookie)
	}
	binding := strings.TrimPrefix(strings.SplitN(cookie, ";", 2)[0], oidcBindingCookie+"=")
	if sha256Hex(binding) != pending.BindingHash {
		t.Fatal("stored binding hash does not match the cookie")
	}
}

func TestOIDCStartRejectsUnavailableProviders(t *testing.T) {
	mux := http.NewServeMux()
	issuer := fakeIssuer(t, mux)
	mux.HandleFunc("/tenant/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oidcMetadata{Issuer: issuer.URL, AuthorizationEndpoint: issuer.URL + "/authorize", TokenEndpoint: issuer.URL + "/token", JWKSURI: issuer.URL + "/jwks"})
	})
	cases := []struct {
		name     string
		provider OIDCProvider
		want     int
	}{
		{name: "disabled", provider: OIDCProvider{ID: "p", Issuer: issuer.URL, ClientID: "c"}, want: http.StatusNotFound},
		{name: "discovery names another issuer", provider: OIDCProvider{ID: "p", Issuer: issuer.URL + "/tenant", ClientID: "c", Enabled: true}, want: http.StatusBadGateway},
		{name: "issuer unreachable", provider: OIDCProvider{ID: "p", Issuer: "http://127.0.0.1:1", ClientID: "c", Enabled: true}, want: http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			login := &OIDCLogin{HTTPClient: issuer.Client(), Deps: func(*gin.Context) (oidcLoginDeps, error) {
				return oidcLoginDeps{Providers: fakeProviders{"p": tc.provider.Normalize()}, States: &fakeStates{}}, nil
			}}
			response := httptest.NewRecorder()
			oidcRouter(login).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/auth/oidc/p/start", nil))
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}

func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/workspace",
		"/workspace/Customer":    "/workspace/Customer",
		"/s/duka/workspace":      "/s/duka/workspace",
		"//evil.example/x":       "/workspace",
		"https://evil.example/x": "/workspace",
		"/\\evil.example":        "/workspace",
		"workspace":              "/workspace",
		"/ok\r\nSet-Cookie: x":   "/workspace",
	} {
		if got := safeReturnTo(in, "/workspace"); got != want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}
