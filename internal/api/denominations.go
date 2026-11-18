package api

import (
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/asenawritescode/kora/internal/numeric"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// Kinds a denomination can take.
const (
	kindNote = "note"
	kindCoin = "coin"
)

// denomination is one note or coin a till counts. Value is exact decimal text.
type denomination struct {
	Value json.Number `json:"value"`
	Kind  string      `json:"kind"`
}

// defaultDenominations is the built-in list per currency code, used until the
// site stores its own. Currencies missing here start with an empty list.
var defaultDenominations = map[string][]denomination{
	"KES": {
		{"1000", kindNote}, {"500", kindNote}, {"200", kindNote}, {"100", kindNote}, {"50", kindNote},
		{"20", kindCoin}, {"10", kindCoin}, {"5", kindCoin}, {"1", kindCoin},
	},
}

// denominationKey is the _kora_site_setting key holding one currency's list.
func denominationKey(currency string) string { return "denominations." + currency }

// validateDenominations checks that every value is positive, every kind is
// note or coin, and the values are unique and sorted largest first. It returns
// the canonical rows to store.
func validateDenominations(lines []denomination) ([]denomination, error) {
	clean := make([]denomination, 0, len(lines))
	var prev decimal.Decimal
	for i, line := range lines {
		value, ok := numeric.Parse(string(line.Value))
		if !ok || !value.IsPositive() {
			return nil, &settingError{"denominations", "validation.invalid_denomination", "Each denomination must be a positive amount"}
		}
		kind := strings.ToLower(strings.TrimSpace(line.Kind))
		if kind != kindNote && kind != kindCoin {
			return nil, &settingError{"denominations", "validation.invalid_denomination_kind", "Each denomination must be a note or a coin"}
		}
		if i > 0 && !value.LessThan(prev) {
			return nil, &settingError{"denominations", "validation.denomination_order", "Denominations must be unique and sorted from largest to smallest"}
		}
		prev = value
		clean = append(clean, denomination{Value: numeric.Number(value), Kind: kind})
	}
	return clean, nil
}

// denominationRow validates a list and returns the setting row that stores it
// for currency.
func denominationRow(currency string, lines []denomination) (siteSetting, error) {
	clean, err := validateDenominations(lines)
	if err != nil {
		return siteSetting{}, err
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return siteSetting{}, err
	}
	return siteSetting{denominationKey(currency), string(raw)}, nil
}

// denominationsFor returns the stored list for currency, or the built-in
// default when the site has not stored one.
func (h *Handler) denominationsFor(c *gin.Context, currency string) ([]denomination, error) {
	var stored string
	err := h.siteTx(c).DB.QueryRow(
		h.siteQuery(c, "SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?"),
		c.GetString("site_name"), denominationKey(currency),
	).Scan(&stored)
	if err == sql.ErrNoRows || err == nil && stored == "" {
		return append([]denomination{}, defaultDenominations[currency]...), nil
	}
	if err != nil {
		return nil, err
	}
	var lines []denomination
	if err := json.Unmarshal([]byte(stored), &lines); err != nil {
		return nil, err
	}
	if lines == nil {
		lines = []denomination{}
	}
	return lines, nil
}

// effectiveCurrency returns the currency a request's denominations apply to:
// the currency it sets, or the site's current currency.
func (h *Handler) effectiveCurrency(c *gin.Context, r siteSettingsRequest) (string, error) {
	if r.Currency != nil {
		return strings.ToUpper(strings.TrimSpace(*r.Currency)), nil
	}
	return h.tenantCurrency(c)
}
