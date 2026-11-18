package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/internal/tenanttime"
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

// readSiteSettings returns the settings body shared by GET and PUT.
func (h *Handler) readSiteSettings(c *gin.Context) (map[string]any, error) {
	currency, err := h.tenantCurrency(c)
	if err != nil {
		return nil, err
	}
	clock, err := tenanttime.Load(c.Request.Context(), h.siteTx(c).DB, h.siteDialect(c), c.GetString("site_name"))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"currency":            currency,
		"currency_precision":  SupportedCurrencies[currency],
		"time_zone":           clock.Location.String(),
		"business_day_cutoff": tenanttime.FormatCutoff(clock.Cutoff),
	}, nil
}

// siteSettingsRequest is a partial update: only fields present are validated
// and stored.
type siteSettingsRequest struct {
	Currency          *string `json:"currency"`
	TimeZone          *string `json:"time_zone"`
	BusinessDayCutoff *string `json:"business_day_cutoff"`
}

// HandleSiteSettings returns safe, non-secret settings for the current site.
// GET /api/system/settings
func (h *Handler) HandleSiteSettings(c *gin.Context) {
	settings, err := h.readSiteSettings(c)
	if err != nil {
		internalError(c, "reading site settings", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: settings})
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
	updates, err := request.updates()
	if err != nil {
		var settingErr *settingError
		if errors.As(err, &settingErr) {
			writeError(c, http.StatusBadRequest, settingErr.code, settingErr.message, map[string]any{"field": settingErr.field})
		} else {
			internalError(c, "validating site settings", err)
		}
		return
	}
	if err = h.saveSiteSettings(c, updates); err != nil {
		internalError(c, "saving site settings", err)
		return
	}
	settings, err := h.readSiteSettings(c)
	if err != nil {
		internalError(c, "reading site settings", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: settings})
}

// siteSetting is one row to upsert into _kora_site_setting.
type siteSetting struct{ key, value string }

// settingError names the request field that failed validation.
type settingError struct{ field, code, message string }

func (e *settingError) Error() string { return e.message }

// updates validates the fields present and returns normalised rows. Absent
// fields are left unchanged.
func (r siteSettingsRequest) updates() ([]siteSetting, error) {
	var rows []siteSetting
	if r.Currency != nil {
		currency := strings.ToUpper(strings.TrimSpace(*r.Currency))
		if _, ok := SupportedCurrencies[currency]; !ok {
			return nil, &settingError{"currency", "validation.invalid_currency", "Choose a supported currency"}
		}
		rows = append(rows, siteSetting{"currency", currency})
	}
	if r.TimeZone != nil {
		zone := strings.TrimSpace(*r.TimeZone)
		if _, err := tenanttime.ParseZone(zone); err != nil {
			return nil, &settingError{"time_zone", "validation.invalid_time_zone", "Choose a valid IANA time zone"}
		}
		rows = append(rows, siteSetting{tenanttime.KeyTimeZone, zone})
	}
	if r.BusinessDayCutoff != nil {
		cutoff, err := tenanttime.ParseCutoff(strings.TrimSpace(*r.BusinessDayCutoff))
		if err != nil {
			return nil, &settingError{"business_day_cutoff", "validation.invalid_business_day_cutoff", "Use a time from 00:00 to 06:00"}
		}
		rows = append(rows, siteSetting{tenanttime.KeyBusinessDayCutoff, tenanttime.FormatCutoff(cutoff)})
	}
	return rows, nil
}

// saveSiteSettings upserts the rows in one transaction.
func (h *Handler) saveSiteSettings(c *gin.Context, updates []siteSetting) error {
	if len(updates) == 0 {
		return nil
	}
	ctx := c.Request.Context()
	site := c.GetString("site_name")
	query := h.siteQuery(c, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)") + " " + h.siteDialect(c).UpsertClause([]string{"site", "setting_key"}, []string{"setting_value"})
	tx, err := h.siteTx(c).DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, query, site, u.key, u.value); err != nil {
			return err
		}
	}
	return tx.Commit()
}
