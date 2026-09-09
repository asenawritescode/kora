package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// SupportedCurrencies is intentionally small and explicit. Currency affects
// financial display across a tenant, so arbitrary values must not reach the UI.
var SupportedCurrencies = map[string]struct{}{
	"AUD": {}, "CAD": {}, "CHF": {}, "CNY": {}, "EUR": {}, "GBP": {},
	"GHS": {}, "INR": {}, "JPY": {}, "KES": {}, "NGN": {}, "RWF": {},
	"TZS": {}, "UGX": {}, "USD": {}, "ZAR": {},
}

type siteSettingsRequest struct {
	Currency string `json:"currency"`
}

// HandleSiteSettings returns safe, non-secret settings for the current site.
// GET /api/system/settings
func (h *Handler) HandleSiteSettings(c *gin.Context) {
	site := c.GetString("site_name")
	currency := "KES"
	var stored string
	err := h.siteTx(c).DB.QueryRow(
		"SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?",
		site, "currency",
	).Scan(&stored)
	if err == nil && stored != "" {
		currency = stored
	} else if err != nil && err != sql.ErrNoRows {
		internalError(c, "reading site settings", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: map[string]string{"currency": currency}})
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
	query := "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?) " + h.TxManager.Dialect.UpsertClause([]string{"site", "setting_key"}, []string{"setting_value"})
	if _, err := db.Exec(query, site, "currency", currency); err != nil {
		internalError(c, "saving site settings", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: map[string]string{"currency": currency}})
}
