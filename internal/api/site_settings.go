package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// SupportedCurrencies maps each allowed tenant currency to its ISO 4217 minor
// units (decimal places). Currency affects financial display across a tenant,
// so arbitrary values must not reach the UI.
var SupportedCurrencies = map[string]int{
	"AUD": 2, "CAD": 2, "CHF": 2, "CNY": 2, "EUR": 2, "GBP": 2,
	"GHS": 2, "INR": 2, "JPY": 0, "KES": 2, "NGN": 2, "RWF": 0,
	"TZS": 2, "UGX": 0, "USD": 2, "ZAR": 2,
}

const defaultTenantCurrency = "KES"

// tenantCurrency returns the site's configured currency, or the default when
// none is stored.
func (h *Handler) tenantCurrency(c *gin.Context) (string, error) {
	var stored string
	err := h.siteTx(c).DB.QueryRow(
		h.siteQuery(c, "SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?"),
		c.GetString("site_name"), "currency",
	).Scan(&stored)
	if err == sql.ErrNoRows || err == nil && stored == "" {
		return defaultTenantCurrency, nil
	}
	return stored, err
}

func currencySettings(currency string) map[string]any {
	return map[string]any{"currency": currency, "currency_precision": SupportedCurrencies[currency]}
}

type siteSettingsRequest struct {
	Currency string `json:"currency"`
}

// HandleSiteSettings returns safe, non-secret settings for the current site.
// GET /api/system/settings
func (h *Handler) HandleSiteSettings(c *gin.Context) {
	currency, err := h.tenantCurrency(c)
	if err != nil {
		internalError(c, "reading site settings", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: currencySettings(currency)})
}

// HandleSiteSettingsUpdate updates tenant-scoped settings. Settings are kept
// separate from infrastructure site_config.yaml and from versioned schema.
// PUT /api/system/settings
func (h *Handler) HandleSiteSettingsUpdate(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var request siteSettingsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid settings", nil)
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(request.Currency))
	if _, ok := SupportedCurrencies[currency]; !ok {
		writeError(c, http.StatusBadRequest, "validation.invalid_currency", "Choose a supported currency", map[string]any{"field": "currency"})
		return
	}

	db := h.siteTx(c).DB
	site := c.GetString("site_name")
	query := h.siteQuery(c, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)") + " " + h.siteDialect(c).UpsertClause([]string{"site", "setting_key"}, []string{"setting_value"})
	if _, err := db.Exec(query, site, "currency", currency); err != nil {
		internalError(c, "saving site settings", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: currencySettings(currency)})
}
