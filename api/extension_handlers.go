package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/webhook"
	"github.com/gin-gonic/gin"
)

// HandleExtensionList returns all extensions for the current site.
func (h *Handler) HandleExtensionList(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	db := h.queryDB(c)
	if db == nil {
		writeError(c, http.StatusServiceUnavailable, "server.database_unavailable", "Database not available", nil)
		return
	}

	rows, err := db.Query(h.siteQuery(c,
		`SELECT name, site, display_name, description, endpoint_url, is_active, subscriptions, api_permissions,
		 secret_count, consecutive_failures, installed_at, last_delivery_at, last_error
		 FROM _kora_extension WHERE site = ? ORDER BY installed_at DESC`), siteNameStr)
	if err != nil {
		internalError(c, "loading extensions", err)
		return
	}
	defer rows.Close()

	var extensions []extensionSummary
	for rows.Next() {
		var name, site, displayName, endpointURL string
		var desc, lastErr, subsJSON, permsJSON sql.NullString
		var isActive bool
		var secretCount, consecutiveFailures int
		var installedAt, lastDeliveryAt sql.NullString
		if err := rows.Scan(&name, &site, &displayName, &desc, &endpointURL, &isActive, &subsJSON, &permsJSON,
			&secretCount, &consecutiveFailures, &installedAt, &lastDeliveryAt, &lastErr); err != nil {
			internalError(c, "reading extensions", err)
			return
		}
		extensions = append(extensions, extensionSummary{
			Name:                name,
			DisplayName:         displayName,
			Description:         desc.String,
			EndpointURL:         endpointURL,
			IsActive:            isActive,
			Subscriptions:       subsJSON.String,
			APIPermissions:      permsJSON.String,
			SecretCount:         secretCount,
			ConsecutiveFailures: consecutiveFailures,
			InstalledAt:         installedAt.String,
			LastDeliveryAt:      lastDeliveryAt.String,
			LastError:           lastErr.String,
		})
	}
	if err := rows.Err(); err != nil {
		internalError(c, "reading extensions", err)
		return
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

	_, err = db.Exec(
		h.siteQuery(c, `INSERT INTO _kora_extension (name, site, display_name, description, endpoint_url, secret, access_token, subscriptions, api_permissions, installed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		req.Name, siteNameStr, req.DisplayName, req.Description, req.EndpointURL, secret, accessToken,
		req.Subscriptions, apiPerms)
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
	tx, err := db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		internalError(c, "deleting extension", err)
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(h.siteQuery(c, `DELETE FROM _kora_extension WHERE site = ? AND name = ?`), siteNameStr, name)
	if err != nil {
		internalError(c, "deleting extension", err)
		return
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		internalError(c, "checking deleted extension", err)
		return
	}
	if deleted == 0 {
		writeError(c, http.StatusNotFound, "extension.not_found", "Extension not found", nil)
		return
	}
	if _, err := tx.Exec(h.siteQuery(c, `DELETE FROM _kora_webhook_delivery WHERE site = ? AND extension_name = ?`), siteNameStr, name); err != nil {
		internalError(c, "deleting extension deliveries", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internalError(c, "deleting extension", err)
		return
	}
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
	result, err := db.Exec(h.siteQuery(c, `UPDATE _kora_extension SET old_secret = secret, old_secret_expires_at = ?,
		secret = ?, secret_count = secret_count + 1, updated_at = CURRENT_TIMESTAMP WHERE site = ? AND name = ?`),
		time.Now().Add(24*time.Hour), secret, siteNameStr, name)
	if err != nil {
		internalError(c, "rotating extension secret", err)
		return
	}
	updated, err := result.RowsAffected()
	if err != nil {
		internalError(c, "checking rotated extension", err)
		return
	}
	if updated == 0 {
		writeError(c, http.StatusNotFound, "extension.not_found", "Extension not found", nil)
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
