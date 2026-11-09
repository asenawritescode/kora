package auth

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// KoraCloudProviderID is the built-in provider for signing in with Kora Cloud.
const KoraCloudProviderID = "kora-cloud"

// KoraCloudIssuerEnv names Kora Cloud's OIDC issuer. When set, every tenant
// offers "Log in with Kora Cloud" as soon as its client secret is stored at
// secret://auth/oidc/kora-cloud/client-secret.
const KoraCloudIssuerEnv = "KORA_CLOUD_OIDC_ISSUER"

type providerLister interface {
	providerReader
	List(ctx context.Context, site string) ([]OIDCProvider, error)
}

// siteProviders layers the built-in Kora Cloud provider under a tenant's own
// provider rows. A stored "kora-cloud" row always wins.
type siteProviders struct {
	rows        providerLister
	secrets     secretReader
	cloudIssuer string
	tenantID    string
}

func newSiteProviders(c *gin.Context, rows providerLister, secrets secretReader, cloudIssuer string) siteProviders {
	tenantID := strings.TrimSpace(c.GetString("site_id"))
	if tenantID == "" {
		tenantID = c.GetString("site_name")
	}
	return siteProviders{rows: rows, secrets: secrets, cloudIssuer: strings.TrimSpace(cloudIssuer), tenantID: tenantID}
}

// koraCloud returns the built-in provider. Its client_id is the tenant's
// stable ID, as registered by Kora Cloud, and it is enabled only once the
// client secret is stored.
func (p siteProviders) koraCloud(site string) (OIDCProvider, bool) {
	if p.cloudIssuer == "" || p.tenantID == "" {
		return OIDCProvider{}, false
	}
	provider := OIDCProvider{
		ID: KoraCloudProviderID, DisplayName: "Kora Cloud", Issuer: p.cloudIssuer,
		ClientID: p.tenantID, Scopes: []string{"openid", "email"},
	}.Normalize()
	if secretValue, err := p.secrets.Get(site, provider.ClientSecretKey); err == nil && secretValue != "" {
		provider.Enabled = true
	}
	return provider, true
}

func (p siteProviders) Get(ctx context.Context, site, id string) (OIDCProvider, error) {
	provider, err := p.rows.Get(ctx, site, id)
	if errors.Is(err, ErrProviderNotFound) && id == KoraCloudProviderID {
		if builtIn, ok := p.koraCloud(site); ok {
			return builtIn, nil
		}
	}
	return provider, err
}

func (p siteProviders) List(ctx context.Context, site string) ([]OIDCProvider, error) {
	providers, err := p.rows.List(ctx, site)
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		if provider.ID == KoraCloudProviderID {
			return providers, nil
		}
	}
	if builtIn, ok := p.koraCloud(site); ok {
		providers = append([]OIDCProvider{builtIn}, providers...)
	}
	return providers, nil
}

func koraCloudIssuerFromEnv() string {
	return strings.TrimSpace(os.Getenv(KoraCloudIssuerEnv))
}
