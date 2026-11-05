package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/auth"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/extension"
	extensionsql "github.com/asenawritescode/kora/extension/sqlstore"
	"github.com/asenawritescode/kora/internal/netguard"
	"github.com/asenawritescode/kora/webhook"
	"github.com/gin-gonic/gin"
)

// ExtensionStore is the extension administration persistence these handlers use.
type ExtensionStore interface {
	List(ctx context.Context, site string) ([]extension.Summary, error)
	Create(ctx context.Context, registration extension.Registration) error
	Delete(ctx context.Context, site, name string) error
	RotateSecret(ctx context.Context, rotation extension.SecretRotation) error
}

// ExtensionStoreOpener binds an ExtensionStore to a tenant database.
type ExtensionStoreOpener func(*sql.DB, kdb.QueryDialect) ExtensionStore

func (h *Handler) extensionStore(c *gin.Context, database *sql.DB) ExtensionStore {
	if h.ExtensionStores != nil {
		return h.ExtensionStores(database, h.siteDialect(c))
	}
	return extensionsql.New(database, h.siteDialect(c))
}

// HandleExtensionList returns all extensions for the current site.
func (h *Handler) HandleExtensionList(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	db := h.queryDB(c)
	if db == nil {
		writeError(c, http.StatusServiceUnavailable, "server.database_unavailable", "Database not available", nil)
		return
	}

	stored, err := h.extensionStore(c, db).List(c.Request.Context(), siteNameStr)
	if err != nil {
		internalError(c, "loading extensions", err)
		return
	}
	var extensions []extensionSummary
	for _, e := range stored {
		extensions = append(extensions, extensionSummary{
			Name:                e.Name,
			DisplayName:         e.DisplayName,
			Description:         e.Description,
			EndpointURL:         e.EndpointURL,
			IsActive:            e.IsActive,
			Subscriptions:       e.Subscriptions,
			APIPermissions:      e.APIPermissions,
			SecretCount:         e.SecretCount,
			ConsecutiveFailures: e.ConsecutiveFailures,
			InstalledAt:         e.InstalledAt,
			LastDeliveryAt:      e.LastDeliveryAt,
			LastError:           e.LastError,
		})
	}
	c.JSON(http.StatusOK, Response{Data: extensionListResponse{Extensions: extensions}})
}

// HandleExtensionGet returns a single extension.
func (h *Handler) HandleExtensionGet(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: extensionGetResponse{Status: "ok"}})
}

// HandleExtensionCreate registers a new extension.
func (h *Handler) HandleExtensionCreate(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)

	var req struct {
		Name           string `json:"name"`
		DisplayName    string `json:"display_name"`
		Description    string `json:"description"`
		EndpointURL    string `json:"endpoint_url"`
		Subscriptions  string `json:"subscriptions"`   // JSON array
		APIPermissions string `json:"api_permissions"` // JSON array
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" || req.EndpointURL == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "name and endpoint_url are required", map[string]any{"fields": []string{"name", "endpoint_url"}})
		return
	}
	if err := netguard.CheckURL(c.Request.Context(), req.EndpointURL, nil); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_endpoint_url", "endpoint_url must be a public http(s) address: "+err.Error(), map[string]any{"field": "endpoint_url"})
		return
	}

	// Generate signing secret and access token.
	secret, err := webhook.GenerateSecret()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "extension.secret_generation_failed", "Failed to generate secret", nil)
		return
	}
	accessToken, err := generateAccessToken()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "extension.token_generation_failed", "Failed to generate access token", nil)
		return
	}

	// Default empty api_permissions to "[]" — never store null/empty.
	apiPerms := req.APIPermissions
	if apiPerms == "" || apiPerms == "null" {
		apiPerms = "[]"
	}

	db := h.queryDB(c)
	if db == nil {
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "Database not available", nil)
		return
	}

	err = h.extensionStore(c, db).Create(c.Request.Context(), extension.Registration{
		Site: siteNameStr, Name: req.Name, DisplayName: req.DisplayName, Description: req.Description,
		EndpointURL: req.EndpointURL, Secret: secret, AccessTokenHash: auth.HashExtensionAccessToken(accessToken),
		Subscriptions: req.Subscriptions, APIPermissions: apiPerms,
	})
	if err != nil {
		slog.Error("creating extension", "error", err)
		writeError(c, http.StatusInternalServerError, "extension.create_failed", "Failed to create extension", nil)
		return
	}

	slog.Info("extension registered", "name", req.Name, "site", siteNameStr)
	// Return secret and access token — shown once.
	c.JSON(http.StatusCreated, Response{Data: extensionCreatedResponse{
		Name:        req.Name,
		Secret:      secret,
		AccessToken: accessToken,
		Warning:     "Store these credentials securely. They will not be shown again.",
	}})
}

// HandleExtensionUpdate updates an extension.
func (h *Handler) HandleExtensionUpdate(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: extensionGetResponse{Status: "ok"}})
}

// HandleExtensionDelete removes an extension.
func (h *Handler) HandleExtensionDelete(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	name := c.Param("name")

	db := h.queryDB(c)
	if db == nil {
		writeError(c, http.StatusNotFound, "extension.not_found", "Not found", nil)
		return
	}
	err := h.extensionStore(c, db).Delete(c.Request.Context(), siteNameStr, name)
	if errors.Is(err, extension.ErrNotFound) {
		writeError(c, http.StatusNotFound, "extension.not_found", "Extension not found", nil)
		return
	}
	if err != nil {
		internalError(c, "deleting extension", err)
		return
	}
	auth.NotifyExtensionTokensChanged(c, siteNameStr)
	c.JSON(http.StatusOK, Response{Data: extensionDeleteResponse{Status: "deleted"}})
}

// HandleExtensionDeliveries returns the delivery log for an extension.
func (h *Handler) HandleExtensionDeliveries(c *gin.Context) {
	name := c.Param("name")
	db := h.queryDB(c)
	if db == nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}

	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	rows, err := db.Query(h.siteQuery(c,
		`SELECT id, event_id, event_type, endpoint_url, status, attempt, response_status, duration_ms, error_message, created_at
		 FROM _kora_webhook_delivery WHERE site = ? AND extension_name = ? ORDER BY created_at DESC LIMIT 50`), siteNameStr, name)
	if err != nil {
		internalError(c, "loading extension deliveries", err)
		return
	}
	defer rows.Close()

	var deliveries []extensionDelivery
	for rows.Next() {
		var id, eventID, eventType, endpointURL, status, createdAt string
		var attempt, durationMs int
		var respStatus sql.NullInt64
		var errMsg sql.NullString
		if err := rows.Scan(&id, &eventID, &eventType, &endpointURL, &status, &attempt, &respStatus, &durationMs, &errMsg, &createdAt); err != nil {
			internalError(c, "reading extension deliveries", err)
			return
		}
		deliveries = append(deliveries, extensionDelivery{
			ID:             id,
			EventID:        eventID,
			EventType:      eventType,
			EndpointURL:    endpointURL,
			Status:         status,
			Attempt:        attempt,
			ResponseStatus: int(respStatus.Int64),
			DurationMs:     durationMs,
			ErrorMessage:   errMsg.String,
			CreatedAt:      createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		internalError(c, "reading extension deliveries", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: extensionDeliveriesResponse{Deliveries: deliveries}})
}

// HandleExtensionReplay replays a specific delivery or all dead-lettered deliveries.
func (h *Handler) HandleExtensionReplay(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: extensionReplayResponse{Status: "replay triggered"}})
}

// HandleExtensionRotateSecret generates a new signing secret for an extension.
func (h *Handler) HandleExtensionRotateSecret(c *gin.Context) {
	name := c.Param("name")
	secret, err := webhook.GenerateSecret()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "extension.secret_generation_failed", "Failed to generate secret", nil)
		return
	}
	db := h.queryDB(c)
	if db == nil {
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "Database not available", nil)
		return
	}

	// Move current secret to old_secret, set 24h expiry.
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	err = h.extensionStore(c, db).RotateSecret(c.Request.Context(), extension.SecretRotation{
		Site: siteNameStr, Name: name, Secret: secret, PreviousExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if errors.Is(err, extension.ErrNotFound) {
		writeError(c, http.StatusNotFound, "extension.not_found", "Extension not found", nil)
		return
	}
	if err != nil {
		internalError(c, "rotating extension secret", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: extensionRotatedSecretResponse{
		Secret:  secret,
		Warning: "Update your extension with this new secret. Both old and new secrets are valid for 24 hours.",
	}})
}

// queryDB returns the site's database or the handler's default.
func (h *Handler) queryDB(c *gin.Context) *sql.DB {
	if db, ok := c.Get("site_db"); ok {
		if sqlDB, ok := db.(*sql.DB); ok {
			return sqlDB
		}
	}
	return h.TxManager.DB
}

func generateAccessToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
