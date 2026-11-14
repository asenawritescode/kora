package auth

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/envcompat"
	"github.com/gin-gonic/gin"
)

// Environment variables that configure Cloud handoff; both are required.
const (
	CloudHandoffURLEnv   = "KORA_ENGINE_CLOUD_IDENTITY_URL"
	CloudHandoffTokenEnv = envcompat.IntegrationTokenEnv

	cloudHandoffPath             = "/api/auth/cloud/handoff"
	cloudHandoffUnconfiguredCode = "auth.cloud_handoff_unconfigured"
)

type CloudHandoffConfig struct {
	RedeemURL    string
	ServiceToken string
	HTTPClient   *http.Client
}

// RegisterCloudHandoffRoute adds the tenant-routed, one-time Cloud identity
// exchange. Engine receives only Cloud identity metadata and creates its own
// tenant session; the session ID is returned only in the HttpOnly cookie.
func RegisterCloudHandoffRoute(router *gin.Engine, sm *SessionManager, db *sql.DB, cfg CloudHandoffConfig) error {
	if router == nil {
		return errors.New("router is required")
	}
	hasURL, hasToken := strings.TrimSpace(cfg.RedeemURL) != "", strings.TrimSpace(cfg.ServiceToken) != ""
	switch {
	case hasURL && !hasToken:
		// The URL turns handoff on; without the shared token it cannot work,
		// so refuse to start instead of silently disabling. The token alone is
		// normal: it is shared with every other Cloud integration.
		return fmt.Errorf("Cloud handoff requires %s with %s; %s is missing", CloudHandoffURLEnv, CloudHandoffTokenEnv, CloudHandoffTokenEnv)
	case !hasURL:
		slog.Warn("Cloud sign-in handoff is disabled: "+CloudHandoffURLEnv+" is not set; Cloud sign-in handoff and member invitation acceptance will not work until it and "+CloudHandoffTokenEnv+" are configured",
			"missing", CloudHandoffURLEnv, "route", cloudHandoffPath, "code", cloudHandoffUnconfiguredCode)
		router.POST(cloudHandoffPath, func(c *gin.Context) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": cloudHandoffUnconfiguredCode, "message": "Cloud sign-in is not configured on this Engine."}})
		})
		return nil
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	router.POST(cloudHandoffPath, func(c *gin.Context) {
		var req struct {
			Code string `json:"code"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Code) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid handoff request"})
			return
		}
		tenant := strings.TrimSpace(c.GetString("site_id"))
		siteName := strings.TrimSpace(c.GetString("site_name"))
		if tenant == "" {
			tenant = siteName
		}
		if tenant == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tenant route is required"})
			return
		}
		payload, _ := json.Marshal(map[string]string{"code": req.Code, "tenant_id": tenant, "audience": "studio"})
		httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, strings.TrimRight(cfg.RedeemURL, "/"), bytes.NewReader(payload))
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "Cloud identity service unavailable"})
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+cfg.ServiceToken)
		response, err := client.Do(httpReq)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "Cloud identity service unavailable"})
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "handoff code is invalid or expired"})
			return
		}
		var identity struct {
			UserID   string `json:"user_id"`
			TenantID string `json:"tenant_id"`
			Audience string `json:"audience"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(c.Writer, response.Body, 8<<10)).Decode(&identity); err != nil || identity.UserID == "" || identity.TenantID != tenant || identity.Audience != "studio" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "handoff identity binding is invalid"})
			return
		}
		targetDB := db
		if value, ok := c.Get("site_db"); ok {
			if routed, ok := value.(*sql.DB); ok && routed != nil {
				targetDB = routed
			}
		}
		if siteName == "" {
			siteName = tenant
		}
		targetSM := sessionManagerForRequest(c, targetDB)
		user, err := targetSM.FindUserByCloudSubject(siteName, identity.UserID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Cloud principal is not provisioned in this tenant"})
			return
		}
		sid, err := targetSM.CreateSession(siteName, user)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create Engine session"})
			return
		}
		cookie := (&http.Cookie{Name: "kora_sid", Value: sid, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: int(SessionLifetime.Seconds()), Expires: time.Now().Add(SessionLifetime)}).String()
		c.Writer.Header().Add("Set-Cookie", cookie)
		// The first authenticated UI action may be a POST (for example, asking
		// for setup suggestions). Mint the double-submit token with the session
		// so that action does not fail before any guarded GET has run.
		ensureCSRFCookie(c)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"name": user.Name, "email": user.Email, "full_name": user.FullName, "roles": user.Roles}})
	})
	return nil
}
