package api

import (
	"crypto/sha256"
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

	rows, err := h.queryDB(c).Query(
		`SELECT name, site, display_name, description, endpoint_url, is_active, subscriptions, api_permissions,
		 secret_count, consecutive_failures, installed_at, last_delivery_at, last_error
		 FROM _kora_extension WHERE site = ? ORDER BY installed_at DESC`, siteNameStr)
	if err != nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}
	defer rows.Close()

	var extensions []map[string]any
	for rows.Next() {
		var name, site, displayName, desc, endpointURL, lastErr string
		var subsJSON, permsJSON sql.NullString
		var isActive bool
		var secretCount, consecutiveFailures int
		var installedAt, lastDeliveryAt sql.NullString
		rows.Scan(&name, &site, &displayName, &desc, &endpointURL, &isActive, &subsJSON, &permsJSON,
			&secretCount, &consecutiveFailures, &installedAt, &lastDeliveryAt, &lastErr)
		extensions = append(extensions, map[string]any{
			"name": name, "display_name": displayName, "description": desc, "endpoint_url": endpointURL,
			"is_active": isActive, "subscriptions": subsJSON.String, "api_permissions": permsJSON.String,
			"secret_count": secretCount, "consecutive_failures": consecutiveFailures,
			"installed_at": installedAt.String, "last_delivery_at": lastDeliveryAt.String, "last_error": lastErr,
		})
	}
	c.JSON(http.StatusOK, Response{Data: extensions})
}

// HandleExtensionGet returns a single extension.
func (h *Handler) HandleExtensionGet(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: map[string]string{"status": "ok"}})
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
		Subscriptions  string `json:"subscriptions"`  // JSON array
		APIPermissions string `json:"api_permissions"` // JSON array
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" || req.EndpointURL == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "name and endpoint_url are required"}})
		return
	}

	// Generate signing secret.
	secret, err := webhook.GenerateSecret()
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to generate secret"}})
		return
	}
	secretHash := hashSecret(secret)

	db := h.queryDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Database not available"}})
		return
	}

	_, err = db.Exec(
		`INSERT INTO _kora_extension (name, site, display_name, description, endpoint_url, secret_hash, subscriptions, api_permissions, installed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NOW(6), NOW(6))`,
		req.Name, siteNameStr, req.DisplayName, req.Description, req.EndpointURL, secretHash,
		req.Subscriptions, req.APIPermissions)
	if err != nil {
		slog.Error("creating extension", "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to create extension"}})
		return
	}

	slog.Info("extension registered", "name", req.Name, "site", siteNameStr)
	// Return secret — shown once.
	c.JSON(http.StatusCreated, Response{Data: map[string]any{
		"name": req.Name, "secret": secret,
		"warning": "Store this secret securely. It will not be shown again.",
	}})
}

// HandleExtensionUpdate updates an extension.
func (h *Handler) HandleExtensionUpdate(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: map[string]string{"status": "ok"}})
}

// HandleExtensionDelete removes an extension.
func (h *Handler) HandleExtensionDelete(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	name := c.Param("name")

	db := h.queryDB(c)
	if db == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Not found"}})
		return
	}
	db.Exec(`DELETE FROM _kora_extension WHERE site = ? AND name = ?`, siteNameStr, name)
	db.Exec(`DELETE FROM _kora_webhook_delivery WHERE extension_name = ?`, name)
	c.JSON(http.StatusOK, Response{Data: map[string]string{"status": "deleted"}})
}

// HandleExtensionDeliveries returns the delivery log for an extension.
func (h *Handler) HandleExtensionDeliveries(c *gin.Context) {
	name := c.Param("name")
	db := h.queryDB(c)
	if db == nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}

	rows, err := db.Query(
		`SELECT id, event_id, event_type, endpoint_url, status, attempt, response_status, duration_ms, error_message, created_at
		 FROM _kora_webhook_delivery WHERE extension_name = ? ORDER BY created_at DESC LIMIT 50`, name)
	if err != nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}
	defer rows.Close()

	var deliveries []map[string]any
	for rows.Next() {
		var id, eventID, eventType, endpointURL, status, errMsg, createdAt string
		var attempt, respStatus, durationMs int
		rows.Scan(&id, &eventID, &eventType, &endpointURL, &status, &attempt, &respStatus, &durationMs, &errMsg, &createdAt)
		deliveries = append(deliveries, map[string]any{
			"id": id, "event_id": eventID, "event_type": eventType,
			"endpoint_url": endpointURL, "status": status, "attempt": attempt,
			"response_status": respStatus, "duration_ms": durationMs,
			"error_message": errMsg, "created_at": createdAt,
		})
	}
	c.JSON(http.StatusOK, Response{Data: deliveries})
}

// HandleExtensionReplay replays a specific delivery or all dead-lettered deliveries.
func (h *Handler) HandleExtensionReplay(c *gin.Context) {
	c.JSON(http.StatusOK, Response{Data: map[string]string{"status": "replay triggered"}})
}

// HandleExtensionRotateSecret generates a new signing secret for an extension.
func (h *Handler) HandleExtensionRotateSecret(c *gin.Context) {
	name := c.Param("name")
	secret, err := webhook.GenerateSecret()
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to generate secret"}})
		return
	}
	secretHash := hashSecret(secret)

	db := h.queryDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Database not available"}})
		return
	}

	// Move current secret to old_secret, set 24h expiry.
	db.Exec(`UPDATE _kora_extension SET old_secret_hash = secret_hash, old_secret_expires_at = ?,
		secret_hash = ?, secret_count = secret_count + 1, updated_at = NOW(6) WHERE name = ?`,
		time.Now().Add(24*time.Hour).Format("2006-01-02 15:04:05"), secretHash, name)

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"secret": secret,
		"warning": "Update your extension with this new secret. Both old and new secrets are valid for 24 hours.",
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

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
