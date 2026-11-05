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
	"testing"
	"time"

	"github.com/asenawritescode/kora/auth/oidc"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

type providerRows []OIDCProvider

func (r providerRows) Get(_ context.Context, _ string, id string) (OIDCProvider, error) {
	for _, p := range r {
		if p.ID == id {
			return p, nil
		}
	}
	return OIDCProvider{}, ErrProviderNotFound
}

func (r providerRows) List(context.Context, string) ([]OIDCProvider, error) { return r, nil }

func tenantContext(tenantID string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_name", "duka")
	c.Set("site_id", tenantID)
	return c
}

func TestKoraCloudProviderIsBuiltInOnceSecretIsStored(t *testing.T) {
	const secretKey = "auth/oidc/kora-cloud/client-secret"
	ctx := context.Background()
	cases := []struct {
		name        string
		rows        providerRows
		secrets     fakeSecrets
		issuer      string
		wantListed  bool
		wantEnabled bool
		wantClient  string
	}{
		{name: "no Cloud issuer configured", secrets: fakeSecrets{secretKey: "s"}},
		{name: "issuer set, secret missing", issuer: "https://cloud.kora.test", wantListed: true, wantClient: "tenant-7f3"},
		{name: "issuer and secret set", issuer: "https://cloud.kora.test", secrets: fakeSecrets{secretKey: "s"}, wantListed: true, wantEnabled: true, wantClient: "tenant-7f3"},
		{name: "stored row overrides built-in", issuer: "https://cloud.kora.test", secrets: fakeSecrets{secretKey: "s"},
			rows: providerRows{OIDCProvider{ID: KoraCloudProviderID, Issuer: "https://cloud.kora.test", ClientID: "custom", Enabled: true}.Normalize()}, wantListed: true, wantEnabled: true, wantClient: "custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providers := newSiteProviders(tenantContext("tenant-7f3"), tc.rows, tc.secrets, tc.issuer)
			list, err := providers.List(ctx, "duka")
			if err != nil {
				t.Fatal(err)
			}
			if listed := len(list) == 1; listed != tc.wantListed {
				t.Fatalf("listed = %v (%+v), want %v", listed, list, tc.wantListed)
			}
			if !tc.wantListed {
				return
			}
			got, err := providers.Get(ctx, "duka", KoraCloudProviderID)
			if err != nil || got.Enabled != tc.wantEnabled || got.ClientID != tc.wantClient || got.ClientSecretKey != secretKey {
				t.Fatalf("get = %+v, %v", got, err)
			}
		})
	}
}

// TestKoraCloudSignInContract runs the full flow against a fake issuer that
// follows Kora Cloud's published OIDC contract.
func TestKoraCloudSignInContract(t *testing.T) {
	const tenantID = "tenant-7f3"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var pending struct{ challenge, nonce, redirect string }
	mux := http.NewServeMux()
	cloud := httptest.NewServer(mux)
	defer cloud.Close()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": cloud.URL, "authorization_endpoint": cloud.URL + "/oauth/authorize",
			"token_endpoint": cloud.URL + "/oauth/token", "jwks_uri": cloud.URL + "/oauth/jwks",
			"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/oauth/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "cloud-2026-11", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		client, secret, _ := r.BasicAuth()
		if client != tenantID || secret != "cloud-secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "cloud-code" ||
			oidc.S256Challenge(r.Form.Get("code_verifier")) != pending.challenge || r.Form.Get("redirect_uri") != pending.redirect {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		now := time.Now()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": cloud.URL, "sub": "cloud-user-42", "aud": tenantID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
			"auth_time": now.Add(-time.Minute).Unix(), "nonce": pending.nonce, "email": "wanjiku@duka.test", "email_verified": true,
		})
		token.Header["kid"] = "cloud-2026-11"
		signed, _ := token.SignedString(key)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque", "token_type": "Bearer", "expires_in": 300, "id_token": signed})
	})

	links := &fakeLinks{}
	states := &fakeStates{}
	secrets := fakeSecrets{"auth/oidc/kora-cloud/client-secret": "cloud-secret"}
	login := &OIDCLogin{HTTPClient: cloud.Client(), PublicAppBaseURL: "https://app.kora.test", CloudIssuer: cloud.URL, Deps: func(c *gin.Context) (oidcLoginDeps, error) {
		return oidcLoginDeps{Providers: newSiteProviders(c, providerRows{}, secrets, cloud.URL), Secrets: secrets, States: states, Links: links, Users: defaultUsers()}, nil
	}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("site_name", "s1"); c.Set("site_id", tenantID) })
	login.Register(router.Group("/api/v1/auth"))

	start := httptest.NewRecorder()
	router.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/kora-cloud/start", nil))
	authorize, _ := url.Parse(start.Header().Get("Location"))
	q := authorize.Query()
	if start.Code != http.StatusFound || authorize.Path != "/oauth/authorize" || q.Get("client_id") != tenantID || q.Get("scope") != "openid email" ||
		q.Get("redirect_uri") != "https://app.kora.test/api/v1/auth/oidc/callback" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize redirect = %d %q", start.Code, start.Header().Get("Location"))
	}
	pending.challenge, pending.nonce, pending.redirect = q.Get("code_challenge"), q.Get("nonce"), q.Get("redirect_uri")

	callback := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?"+url.Values{"code": {"cloud-code"}, "state": {q.Get("state")}}.Encode(), nil)
	callback.Header.Set("Cookie", strings.SplitN(start.Header().Get("Set-Cookie"), ";", 2)[0])
	response := httptest.NewRecorder()
	router.ServeHTTP(response, callback)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/workspace" {
		t.Fatalf("callback = %d %q", response.Code, response.Header().Get("Location"))
	}
	if !strings.Contains(strings.Join(response.Header().Values("Set-Cookie"), "\n"), "kora_sid=sid-USR-1") {
		t.Fatal("no Engine session issued")
	}
	if user, _ := links.Resolve(context.Background(), "s1", cloud.URL, "cloud-user-42"); user != "USR-1" {
		t.Fatalf("link = %q, want USR-1", user)
	}
}
