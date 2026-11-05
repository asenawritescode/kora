package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/secret"
	"github.com/gin-gonic/gin"
)

type authProviderView struct {
	auth.OIDCProvider
	HasClientSecret bool `json:"has_client_secret"`
}

type authProviderRequest struct {
	DisplayName  string   `json:"display_name"`
	Issuer       string   `json:"issuer"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	Scopes       []string `json:"scopes"`
	Enabled      bool     `json:"enabled"`
}

func (h *Handler) authProviderStore(c *gin.Context) auth.SQLAuthProviders {
	return auth.SQLAuthProviders{DB: h.siteTx(c).DB, Dialect: h.siteDialect(c)}
}

func (h *Handler) secretStore(c *gin.Context) *secret.Store {
	return secret.NewStore(h.siteTx(c).DB, h.siteDialect(c))
}

func (h *Handler) hasClientSecret(c *gin.Context, provider auth.OIDCProvider) bool {
	value, err := h.secretStore(c).Get(c.GetString("site_name"), provider.ClientSecretKey)
	return err == nil && value != ""
}

// HandleAuthProviderList lists the tenant's sign-in providers without secrets.
// GET /api/system/auth/providers
func (h *Handler) HandleAuthProviderList(c *gin.Context) {
	providers, err := h.authProviderStore(c).List(c.Request.Context(), c.GetString("site_name"))
	if err != nil {
		internalError(c, "listing sign-in providers", err)
		return
	}
	views := make([]authProviderView, 0, len(providers))
	for _, provider := range providers {
		views = append(views, authProviderView{OIDCProvider: provider, HasClientSecret: h.hasClientSecret(c, provider)})
	}
	c.JSON(http.StatusOK, Response{Data: views})
}

// HandleAuthProviderPut creates or replaces an OIDC provider. The client
// secret is write-only and stored in the secret store.
// PUT /api/system/auth/providers/:id
func (h *Handler) HandleAuthProviderPut(c *gin.Context) {
	var request authProviderRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid sign-in provider", nil)
		return
	}
	site := c.GetString("site_name")
	provider := auth.OIDCProvider{
		ID: c.Param("id"), DisplayName: request.DisplayName, Issuer: request.Issuer,
		ClientID: request.ClientID, Scopes: request.Scopes, Enabled: request.Enabled,
	}.Normalize()
	if err := provider.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_provider", err.Error(), nil)
		return
	}
	if clientSecret := strings.TrimSpace(request.ClientSecret); clientSecret != "" {
		if err := h.secretStore(c).Set(site, provider.ClientSecretKey, clientSecret); err != nil {
			internalError(c, "saving sign-in provider secret", err)
			return
		}
	}
	hasSecret := h.hasClientSecret(c, provider)
	if provider.Enabled && !hasSecret {
		writeError(c, http.StatusBadRequest, "validation.missing_client_secret", "Set a client secret before enabling this provider", map[string]any{"field": "client_secret"})
		return
	}
	saved, err := h.authProviderStore(c).Put(c.Request.Context(), site, provider)
	if err != nil {
		internalError(c, "saving sign-in provider", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: authProviderView{OIDCProvider: saved, HasClientSecret: hasSecret}})
}

// HandleAuthProviderDelete removes a provider and its client secret.
// DELETE /api/system/auth/providers/:id
func (h *Handler) HandleAuthProviderDelete(c *gin.Context) {
	site := c.GetString("site_name")
	ctx := c.Request.Context()
	store := h.authProviderStore(c)
	provider, err := store.Get(ctx, site, c.Param("id"))
	if errors.Is(err, auth.ErrProviderNotFound) {
		writeError(c, http.StatusNotFound, "auth.provider_not_found", "Sign-in provider not found", nil)
		return
	}
	if err != nil {
		internalError(c, "reading sign-in provider", err)
		return
	}
	if provider.Enabled && !h.localSignInAllowed(c) && h.enabledProviderCount(c) <= 1 {
		writeError(c, http.StatusConflict, "auth.last_sign_in_method", "Allow local sign-in before removing the last enabled provider", nil)
		return
	}
	if err := store.Delete(ctx, site, provider.ID); err != nil {
		internalError(c, "deleting sign-in provider", err)
		return
	}
	_ = h.secretStore(c).Delete(site, provider.ClientSecretKey)
	c.Status(http.StatusNoContent)
}

func (h *Handler) localSignInAllowed(c *gin.Context) bool {
	allowed, err := auth.LocalSignInAllowed(c.Request.Context(), h.siteTx(c).DB, h.siteDialect(c), c.GetString("site_name"))
	return err != nil || allowed
}

func (h *Handler) enabledProviderCount(c *gin.Context) int {
	providers, err := h.authProviderStore(c).List(c.Request.Context(), c.GetString("site_name"))
	if err != nil {
		return 0
	}
	count := 0
	for _, provider := range providers {
		if provider.Enabled {
			count++
		}
	}
	return count
}

// HandleAuthPolicy returns the tenant sign-in policy.
// GET /api/system/auth/policy
func (h *Handler) HandleAuthPolicy(c *gin.Context) {
	allowed, err := auth.LocalSignInAllowed(c.Request.Context(), h.siteTx(c).DB, h.siteDialect(c), c.GetString("site_name"))
	if err != nil {
		internalError(c, "reading sign-in policy", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"allow_local_sign_in": allowed}})
}

// HandleAuthPolicyUpdate changes the tenant sign-in policy. Local sign-in can
// only be turned off while at least one provider is enabled.
// PUT /api/system/auth/policy
func (h *Handler) HandleAuthPolicyUpdate(c *gin.Context) {
	var request struct {
		AllowLocalSignIn *bool `json:"allow_local_sign_in"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.AllowLocalSignIn == nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "allow_local_sign_in is required", nil)
		return
	}
	if !*request.AllowLocalSignIn && h.enabledProviderCount(c) == 0 {
		writeError(c, http.StatusConflict, "auth.last_sign_in_method", "Enable a sign-in provider before turning off local sign-in", nil)
		return
	}
	if err := auth.SetLocalSignInAllowed(c.Request.Context(), h.siteTx(c).DB, h.siteDialect(c), c.GetString("site_name"), *request.AllowLocalSignIn); err != nil {
		internalError(c, "saving sign-in policy", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"allow_local_sign_in": *request.AllowLocalSignIn}})
}
