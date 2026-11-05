package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	kdb "github.com/asenawritescode/kora/db"
)

// ErrProviderNotFound reports an unknown or disabled sign-in provider.
var ErrProviderNotFound = errors.New("auth: sign-in provider not found")

// LocalSignInSettingKey stores whether a tenant still accepts local sign-in
// (magic links) in _kora_site_setting. Absent means allowed.
const LocalSignInSettingKey = "auth.allow_local_sign_in"

var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// OIDCProvider is a tenant's OpenID Connect sign-in configuration. The client
// secret itself lives in the secret store under ClientSecretKey.
type OIDCProvider struct {
	ID              string   `json:"id"`
	DisplayName     string   `json:"display_name"`
	Issuer          string   `json:"issuer"`
	ClientID        string   `json:"client_id"`
	ClientSecretKey string   `json:"-"`
	Scopes          []string `json:"scopes"`
	Enabled         bool     `json:"enabled"`
}

// ClientSecretKeyFor is the secret-store key holding a provider's client
// secret; its reference form is secret://auth/oidc/<id>/client-secret.
func ClientSecretKeyFor(providerID string) string {
	return "auth/oidc/" + providerID + "/client-secret"
}

// secretKeyFromRef accepts either a secret-store key or a secret:// reference.
func secretKeyFromRef(ref string) string {
	return strings.TrimPrefix(strings.TrimSpace(ref), "secret://")
}

// Normalize trims fields, fills defaults and always requests the openid scope.
func (p OIDCProvider) Normalize() OIDCProvider {
	p.ID = strings.TrimSpace(p.ID)
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.Issuer = strings.TrimRight(strings.TrimSpace(p.Issuer), "/")
	p.ClientID = strings.TrimSpace(p.ClientID)
	if p.DisplayName == "" {
		p.DisplayName = p.ID
	}
	p.ClientSecretKey = secretKeyFromRef(p.ClientSecretKey)
	if p.ClientSecretKey == "" && p.ID != "" {
		p.ClientSecretKey = ClientSecretKeyFor(p.ID)
	}
	scopes := []string{"openid"}
	for _, scope := range p.Scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" && scope != "openid" && !containsString(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) == 1 {
		scopes = append(scopes, "email", "profile")
	}
	p.Scopes = scopes
	return p
}

// Validate rejects configurations the login flow cannot use safely.
func (p OIDCProvider) Validate() error {
	if !providerIDPattern.MatchString(p.ID) {
		return errors.New("provider id must be 1-64 lowercase letters, digits or hyphens")
	}
	if p.ClientID == "" {
		return errors.New("client_id is required")
	}
	issuer, err := url.Parse(p.Issuer)
	if err != nil || issuer.Host == "" || issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("issuer must be an absolute URL without query or fragment")
	}
	if issuer.Scheme != "https" && !(issuer.Scheme == "http" && isLoopbackHost(issuer.Hostname())) {
		return errors.New("issuer must use https")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func containsString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// SQLAuthProviders persists tenant sign-in providers in _kora_auth_provider.
type SQLAuthProviders struct {
	DB      *sql.DB
	Dialect kdb.Dialect
}

func (s SQLAuthProviders) query(statement string) string {
	if s.Dialect == nil {
		return statement
	}
	return kdb.Rebind(s.Dialect, statement)
}

const authProviderColumns = `id, display_name, issuer, client_id, client_secret_key, scopes, enabled`

func scanAuthProvider(scan func(...any) error) (OIDCProvider, error) {
	var p OIDCProvider
	var scopes string
	if err := scan(&p.ID, &p.DisplayName, &p.Issuer, &p.ClientID, &p.ClientSecretKey, &scopes, &p.Enabled); err != nil {
		return OIDCProvider{}, err
	}
	p.Scopes = strings.Fields(scopes)
	return p, nil
}

// List returns every provider configured for site, enabled or not.
func (s SQLAuthProviders) List(ctx context.Context, site string) ([]OIDCProvider, error) {
	rows, err := s.DB.QueryContext(ctx, s.query(`SELECT `+authProviderColumns+` FROM _kora_auth_provider WHERE site = ? ORDER BY id`), site)
	if err != nil {
		return nil, fmt.Errorf("list sign-in providers: %w", err)
	}
	defer rows.Close()
	var out []OIDCProvider
	for rows.Next() {
		p, err := scanAuthProvider(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("read sign-in provider: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one provider, or ErrProviderNotFound.
func (s SQLAuthProviders) Get(ctx context.Context, site, id string) (OIDCProvider, error) {
	p, err := scanAuthProvider(s.DB.QueryRowContext(ctx, s.query(`SELECT `+authProviderColumns+` FROM _kora_auth_provider WHERE site = ? AND id = ?`), site, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return OIDCProvider{}, ErrProviderNotFound
	}
	if err != nil {
		return OIDCProvider{}, fmt.Errorf("read sign-in provider: %w", err)
	}
	return p, nil
}

// Put creates or replaces a provider after normalizing and validating it.
func (s SQLAuthProviders) Put(ctx context.Context, site string, p OIDCProvider) (OIDCProvider, error) {
	p = p.Normalize()
	if err := p.Validate(); err != nil {
		return OIDCProvider{}, err
	}
	statement := s.query(`INSERT INTO _kora_auth_provider (site, id, display_name, issuer, client_id, client_secret_key, scopes, enabled, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if s.Dialect != nil {
		statement += " " + s.Dialect.UpsertClause([]string{"site", "id"}, []string{"display_name", "issuer", "client_id", "client_secret_key", "scopes", "enabled", "updated_at"})
	}
	if _, err := s.DB.ExecContext(ctx, statement, site, p.ID, p.DisplayName, p.Issuer, p.ClientID, p.ClientSecretKey, strings.Join(p.Scopes, " "), p.Enabled, time.Now().UTC()); err != nil {
		return OIDCProvider{}, fmt.Errorf("save sign-in provider: %w", err)
	}
	return p, nil
}

// Delete removes a provider. Existing identity links are kept so re-adding the
// same issuer restores access without re-linking.
func (s SQLAuthProviders) Delete(ctx context.Context, site, id string) error {
	if _, err := s.DB.ExecContext(ctx, s.query(`DELETE FROM _kora_auth_provider WHERE site = ? AND id = ?`), site, id); err != nil {
		return fmt.Errorf("delete sign-in provider: %w", err)
	}
	return nil
}

// LocalSignInAllowed reports the tenant's local sign-in policy (default true).
func LocalSignInAllowed(ctx context.Context, database *sql.DB, dialect kdb.Dialect, site string) (bool, error) {
	statement := `SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?`
	if dialect != nil {
		statement = kdb.Rebind(dialect, statement)
	}
	var value string
	err := database.QueryRowContext(ctx, statement, site, LocalSignInSettingKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read local sign-in policy: %w", err)
	}
	return value != "false", nil
}

// SetLocalSignInAllowed stores the tenant's local sign-in policy.
func SetLocalSignInAllowed(ctx context.Context, database *sql.DB, dialect kdb.Dialect, site string, allowed bool) error {
	statement := kdb.Rebind(dialect, `INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)`) + " " + dialect.UpsertClause([]string{"site", "setting_key"}, []string{"setting_value"})
	value := "true"
	if !allowed {
		value = "false"
	}
	if _, err := database.ExecContext(ctx, statement, site, LocalSignInSettingKey, value); err != nil {
		return fmt.Errorf("save local sign-in policy: %w", err)
	}
	return nil
}
