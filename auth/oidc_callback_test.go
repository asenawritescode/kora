package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/auth/oidc"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

type fakeLinks struct {
	mu    sync.Mutex
	links map[string]string // issuer|subject -> user
}

func (f *fakeLinks) Resolve(_ context.Context, _, issuer, subject string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if user, ok := f.links[issuer+"|"+subject]; ok {
		return user, nil
	}
	return "", ErrLinkNotFound
}

func (f *fakeLinks) SubjectFor(_ context.Context, _, user, issuer string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, owner := range f.links {
		if owner == user && strings.HasPrefix(key, issuer+"|") {
			return strings.TrimPrefix(key, issuer+"|"), nil
		}
	}
	return "", ErrLinkNotFound
}

func (f *fakeLinks) Link(ctx context.Context, site, user, issuer, subject, _ string, _ time.Time) error {
	if owner, err := f.Resolve(ctx, site, issuer, subject); err == nil {
		if owner == user {
			return nil
		}
		return ErrIdentityCollision
	}
	if existing, err := f.SubjectFor(ctx, site, user, issuer); err == nil && existing != subject {
		return ErrIdentityCollision
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.links == nil {
		f.links = map[string]string{}
	}
	f.links[issuer+"|"+subject] = user
	return nil
}

type fakeUsers map[string]*User

func (f fakeUsers) FindUserByCloudSubject(_, name string) (*User, error) {
	user, ok := f[name]
	if !ok {
		return nil, ErrInvalidCredentials
	}
	if !user.Enabled {
		return nil, ErrDisabledAccount
	}
	return user, nil
}

func (f fakeUsers) FindUserByEmailContext(_ context.Context, _, email string) (*User, error) {
	for _, user := range f {
		if strings.EqualFold(user.Email, email) {
			if !user.Enabled {
				return nil, ErrDisabledAccount
			}
			return user, nil
		}
	}
	return nil, ErrInvalidCredentials
}

func (f fakeUsers) CreateSession(_ string, user *User) (string, error) {
	return "sid-" + user.Name, nil
}

// oidcHarness is a fake issuer plus an Engine wired to it.
type oidcHarness struct {
	t       *testing.T
	issuer  *httptest.Server
	key     *rsa.PrivateKey
	router  *gin.Engine
	links   *fakeLinks
	mu      sync.Mutex
	pending map[string]map[string]string // code -> challenge, nonce, redirect_uri
	claims  func(jwt.MapClaims)
	signer  *rsa.PrivateKey
	now     time.Time
}

func newOIDCHarness(t *testing.T, users fakeUsers) *oidcHarness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	h := &oidcHarness{t: t, key: key, signer: key, links: &fakeLinks{}, pending: map[string]map[string]string{}, now: time.Now().UTC()}
	mux := http.NewServeMux()
	h.issuer = fakeIssuer(t, mux)
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", h.token)
	states := &fakeStates{}
	login := &OIDCLogin{HTTPClient: h.issuer.Client(), PublicAppBaseURL: "https://app.kora.test", Now: func() time.Time { return h.now }, Deps: func(*gin.Context) (oidcLoginDeps, error) {
		return oidcLoginDeps{
			Providers: fakeProviders{"idp": OIDCProvider{ID: "idp", Issuer: h.issuer.URL, ClientID: "studio", Enabled: true}.Normalize()},
			Secrets:   fakeSecrets{ClientSecretKeyFor("idp"): "s3cret"},
			States:    states,
			Links:     h.links,
			Users:     users,
		}, nil
	}}
	h.router = oidcRouter(login)
	return h
}

func (h *oidcHarness) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	user, pass, _ := r.BasicAuth()
	h.mu.Lock()
	issued := h.pending[r.Form.Get("code")]
	delete(h.pending, r.Form.Get("code"))
	h.mu.Unlock()
	if user != "studio" || pass != "s3cret" || issued == nil || oidc.S256Challenge(r.Form.Get("code_verifier")) != issued["challenge"] || r.Form.Get("redirect_uri") != issued["redirect_uri"] {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	claims := jwt.MapClaims{
		"iss": h.issuer.URL, "sub": "sub-1", "aud": "studio", "nonce": issued["nonce"],
		"iat": h.now.Unix(), "exp": h.now.Add(5 * time.Minute).Unix(),
		"email": "wanjiku@duka.test", "email_verified": true,
	}
	if h.claims != nil {
		h.claims(claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "k1"
	signed, err := token.SignedString(h.signer)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"id_token": signed, "token_type": "Bearer"})
}

// signIn runs start → issuer → callback and returns the callback response.
func (h *oidcHarness) signIn(mutate func(callback url.Values, cookie *string)) *httptest.ResponseRecorder {
	h.t.Helper()
	start := httptest.NewRecorder()
	h.router.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/idp/start?return_to=/workspace/Sales%20Order", nil))
	if start.Code != http.StatusFound {
		h.t.Fatalf("start status = %d body = %s", start.Code, start.Body.String())
	}
	location, _ := url.Parse(start.Header().Get("Location"))
	q := location.Query()
	h.mu.Lock()
	h.pending["code-1"] = map[string]string{"challenge": q.Get("code_challenge"), "nonce": q.Get("nonce"), "redirect_uri": q.Get("redirect_uri")}
	h.mu.Unlock()
	cookie := strings.SplitN(start.Header().Get("Set-Cookie"), ";", 2)[0]
	callback := url.Values{"state": {q.Get("state")}, "code": {"code-1"}}
	if mutate != nil {
		mutate(callback, &cookie)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?"+callback.Encode(), nil)
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, request)
	return response
}

func defaultUsers() fakeUsers {
	return fakeUsers{
		"USR-1": {Name: "USR-1", Email: "wanjiku@duka.test", Enabled: true, Roles: []string{"Sales User"}},
		"USR-2": {Name: "USR-2", Email: "otieno@duka.test", Enabled: false},
	}
}

func TestOIDCCallbackSignsInAndLinksByVerifiedEmail(t *testing.T) {
	h := newOIDCHarness(t, defaultUsers())
	response := h.signIn(nil)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/workspace/Sales Order" {
		t.Fatalf("status = %d location = %q", response.Code, response.Header().Get("Location"))
	}
	cookies := strings.Join(response.Header().Values("Set-Cookie"), "\n")
	if !strings.Contains(cookies, "kora_sid=sid-USR-1") || !strings.Contains(cookies, "HttpOnly") || !strings.Contains(cookies, "kora_csrf=") {
		t.Fatalf("cookies = %s", cookies)
	}
	if user, _ := h.links.Resolve(context.Background(), "s1", h.issuer.URL, "sub-1"); user != "USR-1" {
		t.Fatalf("link = %q, want USR-1", user)
	}

	// A returning user is found by subject even after their IdP email changes.
	h.claims = func(c jwt.MapClaims) { c["email"] = "new-address@elsewhere.test"; c["email_verified"] = false }
	if again := h.signIn(nil); again.Code != http.StatusFound || again.Header().Get("Location") != "/workspace/Sales Order" {
		t.Fatalf("returning sign-in location = %q", again.Header().Get("Location"))
	}
}

func TestOIDCCallbackRefusals(t *testing.T) {
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := []struct {
		name   string
		setup  func(*oidcHarness)
		mutate func(url.Values, *string)
		want   string
	}{
		{name: "issuer reported an error", mutate: func(v url.Values, _ *string) { v.Set("error", "access_denied") }, want: ssoErrorDenied},
		{name: "unknown state", mutate: func(v url.Values, _ *string) { v.Set("state", "forged") }, want: ssoErrorInvalidRequest},
		{name: "missing browser binding", mutate: func(_ url.Values, c *string) { *c = "" }, want: ssoErrorInvalidRequest},
		{name: "binding from another browser", mutate: func(_ url.Values, c *string) { *c = oidcBindingCookie + "=attacker" }, want: ssoErrorInvalidRequest},
		{name: "nonce mismatch", setup: func(h *oidcHarness) { h.claims = func(c jwt.MapClaims) { c["nonce"] = "other" } }, want: ssoErrorInvalidToken},
		{name: "wrong audience", setup: func(h *oidcHarness) { h.claims = func(c jwt.MapClaims) { c["aud"] = "another-app" } }, want: ssoErrorInvalidToken},
		{name: "expired token", setup: func(h *oidcHarness) {
			h.claims = func(c jwt.MapClaims) { c["exp"] = h.now.Add(-10 * time.Minute).Unix() }
		}, want: ssoErrorInvalidToken},
		{name: "signed by an unknown key", setup: func(h *oidcHarness) { h.signer = otherKey }, want: ssoErrorInvalidToken},
		{name: "unverified email on first sign-in", setup: func(h *oidcHarness) { h.claims = func(c jwt.MapClaims) { c["email_verified"] = false } }, want: ssoErrorEmailUnverified},
		{name: "no account in this workspace", setup: func(h *oidcHarness) { h.claims = func(c jwt.MapClaims) { c["email"] = "stranger@duka.test" } }, want: ssoErrorNoAccess},
		{name: "disabled account", setup: func(h *oidcHarness) { h.claims = func(c jwt.MapClaims) { c["email"] = "otieno@duka.test" } }, want: ssoErrorDisabled},
		{name: "subject changed after linking", setup: func(h *oidcHarness) {
			_ = h.links.Link(context.Background(), "s1", "USR-1", h.issuer.URL, "sub-old", "wanjiku@duka.test", h.now)
		}, want: ssoErrorConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOIDCHarness(t, defaultUsers())
			if tc.setup != nil {
				tc.setup(h)
			}
			response := h.signIn(tc.mutate)
			want := "/workspace/auth/login?sso_error=" + tc.want
			if response.Code != http.StatusFound || response.Header().Get("Location") != want {
				t.Fatalf("status = %d location = %q, want %q", response.Code, response.Header().Get("Location"), want)
			}
			if strings.Contains(strings.Join(response.Header().Values("Set-Cookie"), "\n"), "kora_sid=") {
				t.Fatal("refused sign-in issued a session")
			}
		})
	}
}

func TestOIDCCallbackStateIsSingleUse(t *testing.T) {
	h := newOIDCHarness(t, defaultUsers())
	var replay url.Values
	var replayCookie string
	first := h.signIn(func(v url.Values, c *string) {
		replay = url.Values{"state": {v.Get("state")}, "code": {"code-1"}}
		replayCookie = *c
	})
	if first.Header().Get("Location") != "/workspace/Sales Order" {
		t.Fatalf("first sign-in location = %q", first.Header().Get("Location"))
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?"+replay.Encode(), nil)
	request.Header.Set("Cookie", replayCookie)
	response := httptest.NewRecorder()
	h.router.ServeHTTP(response, request)
	if response.Header().Get("Location") != "/workspace/auth/login?sso_error="+ssoErrorInvalidRequest {
		t.Fatalf("replayed callback location = %q", response.Header().Get("Location"))
	}
}
