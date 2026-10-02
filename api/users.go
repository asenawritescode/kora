package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"

	"github.com/asenawritescode/kora/auth"
	kdb "github.com/asenawritescode/kora/db"
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
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "No database connection", nil)
		return
	}

	site := siteName(c)
	rows, err := db.Query(
		h.siteQuery(c, "SELECT name, email, full_name, enabled, roles, creation, modified FROM _kora_user WHERE site = ? ORDER BY name"),
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
			internalError(c, "reading user list", err)
			return
		}
		u.Roles = splitRolesStr(rolesStr)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		internalError(c, "reading user list", err)
		return
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
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "No database connection", nil)
		return
	}

	var req UserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request: "+err.Error(), nil)
		return
	}

	// Validate required fields.
	if req.Email == "" || req.FullName == "" || req.Password == "" {
		badRequestError(c, "validation.required_field", "email, full_name, and password are required", map[string]any{"fields": []string{"email", "full_name", "password"}})
		return
	}

	if len(req.Password) < 8 {
		badRequestError(c, "validation.password_too_short", "Password must be at least 8 characters", nil)
		return
	}

	site := siteName(c)

	// Check for duplicate email within this site.
	var count int
	if err := db.QueryRow(h.siteQuery(c, "SELECT COUNT(*) FROM _kora_user WHERE site = ? AND email = ?"), site, req.Email).Scan(&count); err != nil {
		internalError(c, "checking duplicate email", err)
		return
	}
	if count > 0 {
		conflictError(c, "user.email_exists", "A user with this email already exists", map[string]any{"field": "email"})
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
		h.siteQuery(c, "INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, email_verified_at, roles) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, ?)"),
		name, site, req.Email, passwordHash, req.FullName, enabled, rolesStr,
	)
	if err != nil {
		internalError(c, "creating user", err)
		return
	}

	// Fetch the created user to return full response.
	u, err := fetchUser(db, h.siteDialect(c), site, name)
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
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "No database connection", nil)
		return
	}

	site := siteName(c)
	name := c.Param("name")
	u, err := fetchUser(db, h.siteDialect(c), site, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFoundError(c, "user.not_found", "User not found", map[string]any{"name": name})
		} else {
			internalError(c, "loading user", err)
		}
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
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "No database connection", nil)
		return
	}

	site := siteName(c)
	name := c.Param("name")

	// Verify user exists.
	if _, err := fetchUser(db, h.siteDialect(c), site, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFoundError(c, "user.not_found", "User not found", map[string]any{"name": name})
		} else {
			internalError(c, "loading user", err)
		}
		return
	}

	var req UserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request: "+err.Error(), nil)
		return
	}

	var assignments []string
	var args []any
	// Update full_name.
	if req.FullName != "" {
		assignments = append(assignments, "full_name = ?")
		args = append(args, req.FullName)
	}

	// Update roles.
	if req.Roles != nil {
		rolesStr := strings.Join(req.Roles, ",")
		assignments = append(assignments, "roles = ?")
		args = append(args, rolesStr)
	}

	// Update enabled.
	if req.Enabled != nil {
		assignments = append(assignments, "enabled = ?")
		args = append(args, *req.Enabled)
	}

	// Optionally update password.
	if req.Password != "" {
		if len(req.Password) < 8 {
			badRequestError(c, "validation.password_too_short", "Password must be at least 8 characters", nil)
			return
		}
		passwordHash, err := auth.HashPassword(req.Password)
		if err != nil {
			internalError(c, "hashing password", err)
			return
		}
		assignments = append(assignments, "password_hash = ?", "email_verified_at = COALESCE(email_verified_at, CURRENT_TIMESTAMP)")
		args = append(args, passwordHash)
	}
	if len(assignments) > 0 {
		assignments = append(assignments, "modified = CURRENT_TIMESTAMP")
		args = append(args, site, name)
		query := "UPDATE _kora_user SET " + strings.Join(assignments, ", ") + " WHERE site = ? AND name = ?"
		result, err := db.Exec(h.siteQuery(c, query), args...)
		if err != nil {
			internalError(c, "updating user", err)
			return
		}
		updated, err := result.RowsAffected()
		if err != nil {
			internalError(c, "checking updated user", err)
			return
		}
		if updated == 0 {
			notFoundError(c, "user.not_found", "User not found", map[string]any{"name": name})
			return
		}
	}

	// Fetch updated user.
	u, err := fetchUser(db, h.siteDialect(c), site, name)
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
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "No database connection", nil)
		return
	}

	site := siteName(c)
	name := c.Param("name")

	// Prevent self-delete.
	currentUser := c.GetString("user")
	if currentUser == name {
		badRequestError(c, "user.self_delete_forbidden", "You cannot delete your own account", nil)
		return
	}

	// Verify user exists.
	if _, err := fetchUser(db, h.siteDialect(c), site, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFoundError(c, "user.not_found", "User not found", map[string]any{"name": name})
		} else {
			internalError(c, "loading user", err)
		}
		return
	}

	tx, err := db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		internalError(c, "deleting user", err)
		return
	}
	defer tx.Rollback()
	sessionDelete := fmt.Sprintf("DELETE FROM _kora_session WHERE site = ? AND %s = ?", h.siteDialect(c).QuoteIdent("user"))
	if _, err := tx.Exec(h.siteQuery(c, sessionDelete), site, name); err != nil {
		internalError(c, "deleting user sessions", err)
		return
	}
	result, err := tx.Exec(h.siteQuery(c, "DELETE FROM _kora_user WHERE site = ? AND name = ?"), site, name)
	if err != nil {
		internalError(c, "deleting user", err)
		return
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		internalError(c, "checking deleted user", err)
		return
	}
	if deleted == 0 {
		notFoundError(c, "user.not_found", "User not found", map[string]any{"name": name})
		return
	}
	if err := tx.Commit(); err != nil {
		internalError(c, "committing user deletion", err)
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
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "No database connection", nil)
		return
	}

	site := siteName(c)
	name := c.Param("name")

	// Verify user exists.
	if _, err := fetchUser(db, h.siteDialect(c), site, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFoundError(c, "user.not_found", "User not found", map[string]any{"name": name})
		} else {
			internalError(c, "loading user", err)
		}
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
		badRequestError(c, "validation.required_field", "New password is required", map[string]any{"field": "password"})
		return
	}

	if len(req.Password) < 8 {
		badRequestError(c, "validation.password_too_short", "Password must be at least 8 characters", nil)
		return
	}

	// Hash and update password.
	passwordHash, err := auth.HashPassword(req.Password)
	if err != nil {
		internalError(c, "hashing password", err)
		return
	}

	tx, err := db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		internalError(c, "updating password", err)
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(h.siteQuery(c, "UPDATE _kora_user SET password_hash = ?, email_verified_at = COALESCE(email_verified_at, CURRENT_TIMESTAMP), modified = CURRENT_TIMESTAMP WHERE site = ? AND name = ?"), passwordHash, site, name)
	if err != nil {
		internalError(c, "updating password", err)
		return
	}
	updated, err := result.RowsAffected()
	if err != nil {
		internalError(c, "checking updated password", err)
		return
	}
	if updated == 0 {
		notFoundError(c, "user.not_found", "User not found", map[string]any{"name": name})
		return
	}

	// Invalidate all existing sessions for this user on this site so they must re-login.
	sessionDelete := fmt.Sprintf("DELETE FROM _kora_session WHERE site = ? AND %s = ?", h.siteDialect(c).QuoteIdent("user"))
	if _, err := tx.Exec(h.siteQuery(c, sessionDelete), site, name); err != nil {
		internalError(c, "invalidating user sessions", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internalError(c, "committing password reset", err)
		return
	}

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
		Error: ErrorBody{Code: "permission.admin_required", Message: "Administrator role required"},
	})
	return false
}

// fetchUser loads a single user by name and site from the database.
func fetchUser(db *sql.DB, dialect kdb.Dialect, site, name string) (*UserResponse, error) {
	var u UserResponse
	var rolesStr string
	err := db.QueryRow(
		kdb.Rebind(dialect, "SELECT name, email, full_name, enabled, roles, creation, modified FROM _kora_user WHERE site = ? AND name = ?"),
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
