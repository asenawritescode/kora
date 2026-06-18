package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/secret"
)

// SecretEntry represents a secret in list responses (value never exposed).
type SecretEntry struct {
	KeyName   string `json:"key_name"`
	UpdatedAt string `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// HandleSecretList returns all secret key names and timestamps for the current site.
// Values are NEVER returned.
// GET /api/system/secrets
func (h *Handler) HandleSecretList(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}

	db := auth.SiteDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "No database connection"},
		})
		return
	}

	siteName := c.GetString("site_name")
	if siteName == "" {
		siteName = "default"
	}

	rows, err := db.Query(
		"SELECT key_name, updated_at FROM _kora_secret WHERE site = ? ORDER BY key_name",
		siteName,
	)
	if err != nil {
		internalError(c, "secret list query failed", err)
		return
	}
	defer rows.Close()

	var secrets []SecretEntry
	for rows.Next() {
		var s SecretEntry
		if err := rows.Scan(&s.KeyName, &s.UpdatedAt); err != nil {
			continue
		}
		secrets = append(secrets, s)
	}

	if secrets == nil {
		secrets = []SecretEntry{}
	}

	c.JSON(http.StatusOK, Response{Data: secrets})
}

// HandleSecretSet creates or updates a secret.
// POST /api/system/secrets
func (h *Handler) HandleSecretSet(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}

	db := auth.SiteDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "No database connection"},
		})
		return
	}

	siteName := c.GetString("site_name")
	if siteName == "" {
		siteName = "default"
	}

	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request: " + err.Error()},
		})
		return
	}

	if req.Key == "" || req.Value == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "key and value are required"},
		})
		return
	}

	store := secret.NewStore(db)
	if err := store.Set(siteName, req.Key, req.Value); err != nil {
		internalError(c, "setting secret", err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Data: map[string]string{"message": "Secret saved", "key": req.Key},
	})
}

// HandleSecretDelete deletes a secret by key name.
// DELETE /api/system/secrets/:key
func (h *Handler) HandleSecretDelete(c *gin.Context) {
	if !requireAdmin(c) {
		return
	}

	db := auth.SiteDB(c)
	if db == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "No database connection"},
		})
		return
	}

	siteName := c.GetString("site_name")
	if siteName == "" {
		siteName = "default"
	}

	key := c.Param("key")
	if key == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "key is required"},
		})
		return
	}

	store := secret.NewStore(db)
	if err := store.Delete(siteName, key); err != nil {
		internalError(c, "deleting secret", err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Data: map[string]string{"message": "Secret deleted", "key": key},
	})
}
