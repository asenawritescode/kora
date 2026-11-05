package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/auth/oidc"
	"github.com/asenawritescode/kora/secret"
	"github.com/gin-gonic/gin"
)

const (
	oidcLoginLifetime  = 10 * time.Minute
	oidcMetadataTTL    = time.Hour
	oidcBindingCookie  = "kora_oidc_binding"
	oidcCookiePath     = "/api/auth/oidc"
	oidcResponseLimit  = 1 << 20
	defaultAfterSignIn = "/workspace"
)

type providerReader interface {
	Get(ctx context.Context, site, id string) (OIDCProvider, error)
}

type secretReader interface {
	Get(site, key string) (string, error)
}

type loginStateStore interface {
	Save(ctx context.Context, stateHash string, st oidcLoginState) error
	Consume(ctx context.Context, site, stateHash string, now time.Time) (oidcLoginState, error)
}

type identityLinker interface {
	Resolve(ctx context.Context, site, issuer, subject string) (string, error)
	SubjectFor(ctx context.Context, site, user, issuer string) (string, error)
	Link(ctx context.Context, site, user, issuer, subject, email string, linkedAt time.Time) error
}

// tenantUsers is satisfied by *SessionManager. FindUserByCloudSubject looks a
// user up by their tenant user name.
type tenantUsers interface {
	FindUserByCloudSubject(site, name string) (*User, error)
	FindUserByEmailContext(ctx context.Context, site, email string) (*User, error)
	CreateSession(site string, user *User) (string, error)
}

type oidcLoginDeps struct {
	Providers providerReader
	Secrets   secretReader
	States    loginStateStore
	Links     identityLinker
	Users     tenantUsers
}

type oidcMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type cachedMetadata struct {
	doc       oidcMetadata
	fetchedAt time.Time
}

// OIDCLogin serves the OpenID Connect authorization-code + PKCE sign-in flow.
// The Engine stays the session authority: a successful login creates the same
// tenant session as a magic link.
type OIDCLogin struct {
	HTTPClient       *http.Client
	Now              func() time.Time
	PublicAppBaseURL string
	// Deps builds the per-tenant stores for a request. When nil, SQL stores on
	// the routed tenant database are used.
	Deps func(c *gin.Context) (oidcLoginDeps, error)

	mu       sync.Mutex
	metadata map[string]cachedMetadata
}

type tenantUsersFactory func(c *gin.Context, database *sql.DB) tenantUsers

func (l *OIDCLogin) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *OIDCLogin) client() *http.Client {
	if l.HTTPClient != nil {
		return l.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (l *OIDCLogin) deps(c *gin.Context, fallback tenantUsersFactory) (oidcLoginDeps, error) {
	if l.Deps != nil {
		return l.Deps(c)
	}
	database := requestSiteDB(c, nil)
	if database == nil {
		return oidcLoginDeps{}, errors.New("tenant database unavailable")
	}
	dialect := requestDialect(c)
	return oidcLoginDeps{
		Providers: SQLAuthProviders{DB: database, Dialect: dialect},
		Secrets:   secret.NewStore(database, dialect),
		States:    SQLLoginStates{DB: database, Dialect: dialect},
		Links:     SQLIdentityLinks{DB: database, Dialect: dialect},
		Users:     fallback(c, database),
	}, nil
}

// Register mounts the start and callback routes on the public /api/auth group.
func (l *OIDCLogin) Register(group *gin.RouterGroup) {
	users := func(c *gin.Context, database *sql.DB) tenantUsers {
		return sessionManagerForRequest(c, database)
	}
	group.GET("/oidc/:provider/start", func(c *gin.Context) { l.handleStart(c, users) })
}

func (l *OIDCLogin) handleStart(c *gin.Context, users tenantUsersFactory) {
	site := c.GetString("site_name")
	deps, err := l.deps(c, users)
	if site == "" || err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "site.not_found", "message": "Site not found."}})
		return
	}
	ctx := c.Request.Context()
	provider, err := deps.Providers.Get(ctx, site, c.Param("provider"))
	if err != nil || !provider.Enabled || provider.Validate() != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "auth.provider_not_found", "message": "This sign-in provider is not available."}})
		return
	}
	meta, err := l.discover(ctx, provider.Issuer)
	if err != nil {
		slog.Warn("oidc discovery failed", "site", site, "provider", provider.ID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"code": "auth.provider_unavailable", "message": "The sign-in provider is unavailable. Try again shortly."}})
		return
	}
	state, nonce, binding := randomToken(), randomToken(), randomToken()
	pkce, err := oidc.GeneratePKCE()
	if err != nil || state == "" || nonce == "" || binding == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "auth.internal", "message": "Could not start sign-in."}})
		return
	}
	pending := oidcLoginState{
		Site: site, ProviderID: provider.ID, Nonce: nonce, CodeVerifier: pkce.CodeVerifier,
		ReturnTo:    safeReturnTo(c.Query("return_to"), defaultReturnTo(c)),
		BindingHash: sha256Hex(binding), ExpiresAt: l.now().Add(oidcLoginLifetime),
	}
	if err := deps.States.Save(ctx, sha256Hex(state), pending); err != nil {
		slog.Error("saving oidc sign-in request", "site", site, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "auth.internal", "message": "Could not start sign-in."}})
		return
	}
	setOIDCBindingCookie(c, binding, int(oidcLoginLifetime.Seconds()))
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {provider.ClientID},
		"redirect_uri":          {l.redirectURI(c, provider.ID)},
		"scope":                 {strings.Join(provider.Scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {pkce.CodeChallenge},
		"code_challenge_method": {"S256"},
	}
	separator := "?"
	if strings.Contains(meta.AuthorizationEndpoint, "?") {
		separator = "&"
	}
	c.Redirect(http.StatusFound, meta.AuthorizationEndpoint+separator+query.Encode())
}

// discover fetches and caches the issuer's discovery document. The document
// must name the configured issuer exactly.
func (l *OIDCLogin) discover(ctx context.Context, issuer string) (oidcMetadata, error) {
	l.mu.Lock()
	if cached, ok := l.metadata[issuer]; ok && l.now().Sub(cached.fetchedAt) < oidcMetadataTTL {
		l.mu.Unlock()
		return cached.doc, nil
	}
	l.mu.Unlock()
	var doc oidcMetadata
	if err := l.getJSON(ctx, issuer+"/.well-known/openid-configuration", &doc); err != nil {
		return oidcMetadata{}, err
	}
	if strings.TrimRight(doc.Issuer, "/") != issuer {
		return oidcMetadata{}, fmt.Errorf("discovery issuer %q does not match %q", doc.Issuer, issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return oidcMetadata{}, errors.New("discovery document is missing endpoints")
	}
	l.mu.Lock()
	if l.metadata == nil {
		l.metadata = make(map[string]cachedMetadata)
	}
	l.metadata[issuer] = cachedMetadata{doc: doc, fetchedAt: l.now()}
	l.mu.Unlock()
	return doc, nil
}

func (l *OIDCLogin) getJSON(ctx context.Context, target string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := l.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d", target, response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, oidcResponseLimit)).Decode(out)
}

func (l *OIDCLogin) redirectURI(c *gin.Context, providerID string) string {
	origin := strings.TrimRight(strings.TrimSpace(l.PublicAppBaseURL), "/")
	if origin == "" {
		scheme := "https"
		if c.Request.TLS == nil && c.GetHeader("X-Forwarded-Proto") != "https" {
			scheme = "http"
		}
		origin = scheme + "://" + c.Request.Host
	}
	return origin + oidcCookiePath + "/" + providerID + "/callback"
}

// safeReturnTo accepts only same-origin absolute paths.
func safeReturnTo(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\\\r\n") {
		return fallback
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return fallback
	}
	return value
}

func defaultReturnTo(c *gin.Context) string {
	return strings.TrimSuffix(magicLinkLoginPath(c), "/auth/login")
}

func randomToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func setOIDCBindingCookie(c *gin.Context, value string, maxAge int) {
	cookie := &http.Cookie{Name: oidcBindingCookie, Value: value, Path: oidcCookiePath, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
	if maxAge < 0 {
		cookie.Value = ""
		cookie.Expires = time.Unix(1, 0)
	}
	c.Writer.Header().Add("Set-Cookie", cookie.String())
}
