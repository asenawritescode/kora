package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/doctype"
)

// --- Request / Response types ---

// UserRequest is the request body for create/update user.
type UserRequest struct {
	Email    string   `json:"email"`
	Password string   `json:"password,omitempty"`
	FullName string   `json:"full_name"`
	Roles    []string `json:"roles"`
	Enabled  *bool    `json:"enabled,omitempty"`
}

// UserResponse is the public representation of a user (no password_hash).
type UserResponse struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	FullName string   `json:"full_name"`
	Roles    []string `json:"roles"`
	Enabled  bool     `json:"enabled"`
	Created  string   `json:"created"`
	Modified string   `json:"modified"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// siteName extracts the current site name from the Gin context.
func siteName(c *gin.Context) string {
	s, _ := c.Get("site_name")
	if name, ok := s.(string); ok {
		return name
	}
	return ""
}

// HandleUserList returns all users for the current site.
// GET /api/system/users
func (h *Handler) HandleUserList(c *gin.Context) {
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

	site := siteName(c)
	rows, err := db.Query(
		"SELECT name, email, full_name, enabled, roles, creation, modified FROM _kora_user WHERE site = ? ORDER BY name",
		site,
	)
	if err != nil {
		internalError(c, "user list query failed", err)
		return
	}
	defer rows.Close()

	var users []UserResponse
	for rows.Next() {
		var u UserResponse
		var rolesStr string
		if err := rows.Scan(&u.Name, &u.Email, &u.FullName, &u.Enabled, &rolesStr, &u.Created, &u.Modified); err != nil {
			continue
		}
		u.Roles = splitRolesStr(rolesStr)
		users = append(users, u)
	}

	if users == nil {
		users = []UserResponse{}
	}

	c.JSON(http.StatusOK, Response{Data: users})
}

// HandleUserCreate creates a new user.
// POST /api/system/users
func (h *Handler) HandleUserCreate(c *gin.Context) {
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

	var req UserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request: " + err.Error()},
		})
		return
	}

	// Validate required fields.
	if req.Email == "" || req.FullName == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "email, full_name, and password are required"},
		})
		return
	}

	if len(req.Password) < 8 {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Password must be at least 8 characters"},
		})
		return
	}

	site := siteName(c)

	// Check for duplicate email within this site.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM _kora_user WHERE site = ? AND email = ?", site, req.Email).Scan(&count); err != nil {
		internalError(c, "checking duplicate email", err)
		return
	}
	if count > 0 {
		c.JSON(http.StatusConflict, ErrorResponse{
			Error: map[string]string{"message": "A user with this email already exists", "field": "email"},
		})
		return
	}

	// Generate ULID and hash password.
	name := ulid.Make().String()
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		internalError(c, "hashing password", err)
		return
	}

	rolesStr := strings.Join(req.Roles, ",")
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	_, err = db.Exec(
		"INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, roles) VALUES (?, ?, ?, ?, ?, ?, ?)",
		name, site, req.Email, passwordHash, req.FullName, enabled, rolesStr,
	)
	if err != nil {
		internalError(c, "creating user", err)
		return
	}

	// Fetch the created user to return full response.
	u, err := fetchUser(db, site, name)
	if err != nil {
		internalError(c, "fetching created user", err)
		return
	}

	c.JSON(http.StatusCreated, Response{Data: u})
}

// HandleUserGet returns a single user by name (ULID).
// GET /api/system/users/:name
func (h *Handler) HandleUserGet(c *gin.Context) {
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

	site := siteName(c)
	name := c.Param("name")
	u, err := fetchUser(db, site, name)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "User not found"},
		})
		return
	}

	c.JSON(http.StatusOK, Response{Data: u})
}

// HandleUserUpdate updates a user's profile fields.
// PUT /api/system/users/:name
func (h *Handler) HandleUserUpdate(c *gin.Context) {
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

	site := siteName(c)
	name := c.Param("name")

	// Verify user exists.
	if _, err := fetchUser(db, site, name); err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "User not found"},
		})
		return
	}

	var req UserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request: " + err.Error()},
		})
		return
	}

	// Update full_name.
	if req.FullName != "" {
		db.Exec("UPDATE _kora_user SET full_name = ?, modified = CURRENT_TIMESTAMP WHERE site = ? AND name = ?", req.FullName, site, name)
	}

	// Update roles.
	if req.Roles != nil {
		rolesStr := strings.Join(req.Roles, ",")
		db.Exec("UPDATE _kora_user SET roles = ?, modified = CURRENT_TIMESTAMP WHERE site = ? AND name = ?", rolesStr, site, name)
	}

	// Update enabled.
	if req.Enabled != nil {
		db.Exec("UPDATE _kora_user SET enabled = ?, modified = CURRENT_TIMESTAMP WHERE site = ? AND name = ?", *req.Enabled, site, name)
	}

	// Optionally update password.
	if req.Password != "" {
		if len(req.Password) < 8 {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: map[string]string{"message": "Password must be at least 8 characters"},
			})
			return
		}
		passwordHash, err := auth.HashPassword(req.Password)
		if err != nil {
			internalError(c, "hashing password", err)
			return
		}
		db.Exec("UPDATE _kora_user SET password_hash = ?, modified = CURRENT_TIMESTAMP WHERE site = ? AND name = ?", passwordHash, site, name)
	}

	// Fetch updated user.
	u, err := fetchUser(db, site, name)
	if err != nil {
		internalError(c, "fetching updated user", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: u})
}

// HandleUserDelete deletes a user and their sessions.
// DELETE /api/system/users/:name
func (h *Handler) HandleUserDelete(c *gin.Context) {
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

	site := siteName(c)
	name := c.Param("name")

	// Prevent self-delete.
	currentUser := c.GetString("user")
	if currentUser == name {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "You cannot delete your own account"},
		})
		return
	}

	// Verify user exists.
	if _, err := fetchUser(db, site, name); err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "User not found"},
		})
		return
	}

	// Delete sessions for this user on this site.
	db.Exec("DELETE FROM _kora_session WHERE site = ? AND user = ?", site, name)

	// Delete user.
	if _, err := db.Exec("DELETE FROM _kora_user WHERE site = ? AND name = ?", site, name); err != nil {
		internalError(c, "deleting user", err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Data: map[string]string{"message": "User deleted"},
	})
}

// HandleUserResetPassword sets a new password for a user and invalidates all their sessions.
// POST /api/system/users/:name/reset-password
func (h *Handler) HandleUserResetPassword(c *gin.Context) {
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

	site := siteName(c)
	name := c.Param("name")

	// Verify user exists.
	if _, err := fetchUser(db, site, name); err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "User not found"},
		})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "New password is required"},
		})
		return
	}

	if len(req.Password) < 8 {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Password must be at least 8 characters"},
		})
		return
	}

	// Hash and update password.
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		internalError(c, "hashing password", err)
		return
	}

	if _, err := db.Exec("UPDATE _kora_user SET password_hash = ?, modified = CURRENT_TIMESTAMP WHERE site = ? AND name = ?", passwordHash, site, name); err != nil {
		internalError(c, "updating password", err)
		return
	}

	// Invalidate all existing sessions for this user on this site so they must re-login.
	db.Exec("DELETE FROM _kora_session WHERE site = ? AND user = ?", site, name)

	c.JSON(http.StatusOK, Response{
		Data: map[string]string{"message": "Password reset. User must log in again."},
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// requireAdmin checks if the current user has the admin role.
// Returns false and writes 403 if not an admin.
func requireAdmin(c *gin.Context) bool {
	roles := c.GetStringSlice("user_roles")
	for _, r := range roles {
		if r == doctype.AdminRole {
			return true
		}
	}
	c.JSON(http.StatusForbidden, ErrorResponse{
		Error: map[string]string{"message": "Administrator role required"},
	})
	return false
}

// fetchUser loads a single user by name and site from the database.
func fetchUser(db *sql.DB, site, name string) (*UserResponse, error) {
	var u UserResponse
	var rolesStr string
	err := db.QueryRow(
		"SELECT name, email, full_name, enabled, roles, creation, modified FROM _kora_user WHERE site = ? AND name = ?",
		site, name,
	).Scan(&u.Name, &u.Email, &u.FullName, &u.Enabled, &rolesStr, &u.Created, &u.Modified)
	if err != nil {
		return nil, err
	}
	u.Roles = splitRolesStr(rolesStr)
	return &u, nil
}

// splitRolesStr splits a comma or newline separated roles string into a slice.
func splitRolesStr(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
