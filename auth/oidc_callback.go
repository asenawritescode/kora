package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/asenawritescode/kora/auth/oidc"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Sign-in failure codes passed to the login page as ?sso_error=.
const (
	ssoErrorDenied          = "denied"
	ssoErrorInvalidRequest  = "invalid_request"
	ssoErrorUnavailable     = "provider_unavailable"
	ssoErrorInvalidToken    = "invalid_token"
	ssoErrorEmailUnverified = "email_unverified"
	ssoErrorNoAccess        = "no_access"
	ssoErrorConflict        = "identity_conflict"
	ssoErrorDisabled        = "account_disabled"
)

type signInError struct {
	code  string
	cause error
}

func (e *signInError) Error() string { return e.code + ": " + fmt.Sprint(e.cause) }

func failSignIn(code string, cause error) error { return &signInError{code: code, cause: cause} }

func (l *OIDCLogin) handleCallback(c *gin.Context, users tenantUsersFactory) {
	setOIDCBindingCookie(c, "", -1)
	site := c.GetString("site_name")
	user, providerID, returnTo, err := l.completeSignIn(c, users)
	if err != nil {
		code := ssoErrorInvalidRequest
		var failure *signInError
		if errors.As(err, &failure) {
			code = failure.code
		}
		slog.Warn("oidc sign-in refused", "site", site, "provider", providerID, "reason", code, "error", err)
		c.Redirect(http.StatusFound, magicLinkLoginPath(c)+"?sso_error="+code)
		return
	}
	slog.Info("audit: sign-in", "method", "oidc", "site", site, "provider", providerID, "user", user.Name)
	c.Redirect(http.StatusFound, returnTo)
}

func (l *OIDCLogin) completeSignIn(c *gin.Context, users tenantUsersFactory) (*User, string, string, error) {
	providerID := ""
	if c.Query("error") != "" {
		return nil, providerID, "", failSignIn(ssoErrorDenied, errors.New(c.Query("error")))
	}
	site := c.GetString("site_name")
	deps, err := l.deps(c, users)
	if site == "" || err != nil {
		return nil, providerID, "", failSignIn(ssoErrorInvalidRequest, err)
	}
	ctx := c.Request.Context()
	binding, _ := c.Cookie(oidcBindingCookie)
	state, code := c.Query("state"), c.Query("code")
	if binding == "" || state == "" || code == "" {
		return nil, providerID, "", failSignIn(ssoErrorInvalidRequest, errors.New("missing state, code or browser binding"))
	}
	pending, err := deps.States.Consume(ctx, site, sha256Hex(state), l.now())
	if err != nil {
		return nil, providerID, "", failSignIn(ssoErrorInvalidRequest, err)
	}
	providerID = pending.ProviderID
	if subtle.ConstantTimeCompare([]byte(sha256Hex(binding)), []byte(pending.BindingHash)) != 1 {
		return nil, providerID, "", failSignIn(ssoErrorInvalidRequest, errors.New("sign-in request was started in another browser"))
	}
	provider, err := deps.Providers.Get(ctx, site, pending.ProviderID)
	if err != nil || !provider.Enabled {
		return nil, providerID, "", failSignIn(ssoErrorUnavailable, err)
	}
	clientSecret, err := deps.Secrets.Get(site, provider.ClientSecretKey)
	if err != nil || clientSecret == "" {
		return nil, providerID, "", failSignIn(ssoErrorUnavailable, errors.New("client secret not configured"))
	}
	meta, err := l.discover(ctx, provider.Issuer)
	if err != nil {
		return nil, providerID, "", failSignIn(ssoErrorUnavailable, err)
	}
	rawIDToken, err := l.exchangeCode(ctx, meta.TokenEndpoint, provider.ClientID, clientSecret, code, l.redirectURI(c), pending.CodeVerifier)
	if err != nil {
		return nil, providerID, "", failSignIn(ssoErrorUnavailable, err)
	}
	claims, err := l.verifyIDToken(ctx, meta, provider, rawIDToken, pending.Nonce)
	if err != nil {
		return nil, providerID, "", failSignIn(ssoErrorInvalidToken, err)
	}
	user, err := l.resolveUser(ctx, deps, site, claims)
	if err != nil {
		return nil, providerID, "", err
	}
	sid, err := deps.Users.CreateSession(site, user)
	if err != nil {
		return nil, providerID, "", failSignIn(ssoErrorUnavailable, err)
	}
	c.Writer.Header().Add("Set-Cookie", (&http.Cookie{Name: "kora_sid", Value: sid, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: int(SessionLifetime.Seconds()), Expires: time.Now().Add(SessionLifetime)}).String())
	ensureCSRFCookie(c)
	return user, providerID, pending.ReturnTo, nil
}

// exchangeCode redeems the authorization code with client_secret_basic and
// the PKCE verifier, returning the raw ID token.
func (l *OIDCLogin) exchangeCode(ctx context.Context, tokenEndpoint, clientID, clientSecret, code, redirectURI, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"client_id":     {clientID},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))
	response, err := l.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer response.Body.Close()
	var body struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, oidcResponseLimit)).Decode(&body); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if response.StatusCode != http.StatusOK || body.IDToken == "" {
		return "", fmt.Errorf("token endpoint returned %d %s", response.StatusCode, body.Error)
	}
	return body.IDToken, nil
}

// verifyIDToken checks the signature against the issuer's JWKS, then issuer,
// audience, expiry and nonce.
func (l *OIDCLogin) verifyIDToken(ctx context.Context, meta oidcMetadata, provider OIDCProvider, raw, nonce string) (oidc.IDTokenClaims, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		return l.signingKey(ctx, provider.Issuer, meta.JWKSURI, kid)
	},
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithIssuer(provider.Issuer),
		jwt.WithAudience(provider.ClientID),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(time.Minute),
		jwt.WithTimeFunc(l.now),
	)
	if err != nil {
		return oidc.IDTokenClaims{}, err
	}
	if audiences, _ := claims.GetAudience(); len(audiences) > 1 {
		if azp, _ := claims["azp"].(string); azp != provider.ClientID {
			return oidc.IDTokenClaims{}, oidc.ErrAudienceMismatch
		}
	}
	parsed := oidc.IDTokenClaims{Issuer: provider.Issuer, Audience: provider.ClientID}
	parsed.Subject, _ = claims["sub"].(string)
	parsed.Email, _ = claims["email"].(string)
	parsed.Name, _ = claims["name"].(string)
	parsed.Nonce, _ = claims["nonce"].(string)
	switch verified := claims["email_verified"].(type) {
	case bool:
		parsed.EmailVerified = verified
	case string:
		parsed.EmailVerified = verified == "true"
	}
	if parsed.Subject == "" {
		return oidc.IDTokenClaims{}, errors.New("id token has no subject")
	}
	if _, err := oidc.ValidateIDToken(parsed, oidc.ValidateOptions{ExpectedIssuer: provider.Issuer, ExpectedAudience: provider.ClientID, ExpectedNonce: nonce, Now: l.now()}); err != nil {
		return oidc.IDTokenClaims{}, err
	}
	return parsed, nil
}

// resolveUser maps verified claims to an existing tenant user. A known
// (issuer, subject) wins; otherwise a verified email may link to an existing,
// enabled user who has no other subject for this issuer. Users are never
// created here.
func (l *OIDCLogin) resolveUser(ctx context.Context, deps oidcLoginDeps, site string, claims oidc.IDTokenClaims) (*User, error) {
	name, err := deps.Links.Resolve(ctx, site, claims.Issuer, claims.Subject)
	if err == nil {
		user, err := deps.Users.FindUserByCloudSubject(site, name)
		if errors.Is(err, ErrDisabledAccount) {
			return nil, failSignIn(ssoErrorDisabled, err)
		}
		if err != nil {
			return nil, failSignIn(ssoErrorNoAccess, err)
		}
		return user, nil
	}
	if !errors.Is(err, ErrLinkNotFound) {
		return nil, failSignIn(ssoErrorUnavailable, err)
	}
	if claims.Email == "" || !claims.EmailVerified {
		return nil, failSignIn(ssoErrorEmailUnverified, errors.New("first sign-in requires a verified email"))
	}
	user, err := deps.Users.FindUserByEmailContext(ctx, site, claims.Email)
	if errors.Is(err, ErrDisabledAccount) {
		return nil, failSignIn(ssoErrorDisabled, err)
	}
	if err != nil {
		return nil, failSignIn(ssoErrorNoAccess, err)
	}
	if err := deps.Links.Link(ctx, site, user.Name, claims.Issuer, claims.Subject, claims.Email, l.now()); err != nil {
		if errors.Is(err, ErrIdentityCollision) {
			return nil, failSignIn(ssoErrorConflict, err)
		}
		return nil, failSignIn(ssoErrorUnavailable, err)
	}
	slog.Info("audit: identity linked", "site", site, "user", user.Name, "issuer", claims.Issuer)
	return user, nil
}
