package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

var experienceHexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

var approvedExperienceFonts = map[string]struct{}{
	"Geist": {}, "Inter": {}, "IBM Plex Sans": {}, "Source Sans 3": {}, "system": {},
}

// resolveTenantExperience returns safe tenant settings with neutral defaults.
// It is deliberately independent of Kora's global console branding.
func (h *Handler) resolveTenantExperience(c *gin.Context) TenantExperience {
	siteName := c.GetString("site_name")
	branding := Branding{
		AppName:      experienceSiteLabel(siteName),
		ShortName:    experienceShortName(siteName),
		PrimaryColor: "#334155",
		AccentColor:  "#0f766e",
		FontFamily:   "Geist",
		DefaultMode:  "light",
	}
	copy := defaultExperienceCopy

	if siteName == "" {
		return TenantExperience{Branding: branding, Copy: copy}
	}
	for key, target := range map[string]*string{
		"branding.app_name": &branding.AppName, "branding.short_name": &branding.ShortName,
		"branding.logo_url": &branding.LogoURL, "branding.logo_dark_url": &branding.LogoDarkURL,
		"branding.favicon_url": &branding.FaviconURL, "branding.primary_color": &branding.PrimaryColor,
		"branding.accent_color": &branding.AccentColor, "branding.font_family": &branding.FontFamily,
		"branding.heading_font": &branding.HeadingFont, "branding.default_mode": &branding.DefaultMode,
		"copy.login_title": &copy.LoginTitle, "copy.login_description": &copy.LoginDescription,
		"copy.login_help": &copy.LoginHelp, "copy.workspace_loading": &copy.WorkspaceLoading,
		"copy.empty_records": &copy.EmptyRecords, "copy.support_label": &copy.SupportLabel,
	} {
		if value, err := h.readTenantExperienceSetting(c, siteName, key); err == nil && value != "" {
			*target = value
		}
	}
	return TenantExperience{Branding: branding, Copy: copy}
}

func (h *Handler) readTenantExperienceSetting(c *gin.Context, siteName, key string) (string, error) {
	var value string
	err := h.siteTx(c).DB.QueryRow(
		"SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?",
		siteName, key,
	).Scan(&value)
	return value, err
}

func (h *Handler) HandleSystemBranding(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: h.resolveTenantExperience(c)})
}

type experienceUpdateRequest struct {
	Branding Branding       `json:"branding"`
	Copy     ExperienceCopy `json:"copy"`
}

func (h *Handler) HandleSystemExperienceUpdate(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}
	var request experienceUpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid experience settings", nil)
		return
	}
	if err := validateExperience(request.Branding); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_branding", err.Error(), nil)
		return
	}
	values := map[string]string{
		"branding.app_name": request.Branding.AppName, "branding.short_name": request.Branding.ShortName,
		"branding.logo_url": request.Branding.LogoURL, "branding.logo_dark_url": request.Branding.LogoDarkURL,
		"branding.favicon_url": request.Branding.FaviconURL, "branding.primary_color": request.Branding.PrimaryColor,
		"branding.accent_color": request.Branding.AccentColor, "branding.font_family": request.Branding.FontFamily,
		"branding.heading_font": request.Branding.HeadingFont, "branding.default_mode": request.Branding.DefaultMode,
		"copy.login_title": request.Copy.LoginTitle, "copy.login_description": request.Copy.LoginDescription,
		"copy.login_help": request.Copy.LoginHelp, "copy.workspace_loading": request.Copy.WorkspaceLoading,
		"copy.empty_records": request.Copy.EmptyRecords, "copy.support_label": request.Copy.SupportLabel,
	}
	db := h.siteTx(c).DB
	siteName := c.GetString("site_name")
	query := "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?) " + h.TxManager.Dialect.UpsertClause([]string{"site", "setting_key"}, []string{"setting_value"})
	for key, value := range values {
		if len(value) > 255 {
			writeError(c, http.StatusBadRequest, "validation.branding_value_too_long", "Branding values must be 255 characters or fewer", map[string]any{"field": key})
			return
		}
		if _, err := db.Exec(query, siteName, key, strings.TrimSpace(value)); err != nil {
			internalError(c, "saving tenant experience", err)
			return
		}
	}
	c.JSON(http.StatusOK, Response{Data: h.resolveTenantExperience(c)})
}

func validateExperience(branding Branding) error {
	for label, value := range map[string]string{"primary color": branding.PrimaryColor, "accent color": branding.AccentColor} {
		if value != "" && !experienceHexColor.MatchString(value) {
			return fmt.Errorf("%s must be a six-digit hexadecimal color", label)
		}
	}
	if branding.DefaultMode != "" && branding.DefaultMode != "light" && branding.DefaultMode != "dark" && branding.DefaultMode != "system" {
		return fmt.Errorf("default mode must be light, dark, or system")
	}
	if branding.FontFamily != "" {
		if _, ok := approvedExperienceFonts[branding.FontFamily]; !ok {
			return fmt.Errorf("font family is not supported")
		}
	}
	if branding.HeadingFont != "" {
		if _, ok := approvedExperienceFonts[branding.HeadingFont]; !ok {
			return fmt.Errorf("heading font is not supported")
		}
	}
	for label, value := range map[string]string{"logo URL": branding.LogoURL, "dark logo URL": branding.LogoDarkURL, "favicon URL": branding.FaviconURL} {
		if value != "" && !strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "https://") {
			return fmt.Errorf("%s must be a same-origin path or HTTPS URL", label)
		}
	}
	return nil
}

func experienceSiteLabel(siteName string) string {
	if siteName == "" {
		return "Workspace"
	}
	part := strings.Split(siteName, ".")[0]
	part = strings.ReplaceAll(strings.ReplaceAll(part, "-", " "), "_", " ")
	if part == "" {
		return "Workspace"
	}
	return strings.Title(part) + " Workspace"
}

func experienceShortName(siteName string) string {
	if siteName == "" {
		return "Workspace"
	}
	part := strings.Split(strings.Split(siteName, ".")[0], "-")[0]
	part = strings.ReplaceAll(part, "_", " ")
	if part == "" {
		return "Workspace"
	}
	return strings.Title(part)
}
