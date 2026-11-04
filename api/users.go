package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// --- Request / Response types ---

// UserAccessUpdate is the request body for tenant-scoped access changes.
// Cloud owns account identity and organization membership; Engine owns only
// workspace roles and whether a provisioned principal is enabled here.
type UserAccessUpdate struct {
	Roles   []string `json:"roles"`
	Enabled *bool    `json:"enabled,omitempty"`
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

// UserDirectoryEntry is the minimum tenant-scoped identity shown in user
// pickers. Administrative roles and account metadata are intentionally omitted.
type UserDirectoryEntry struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
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

// HandleUserDirectory returns a bounded list of enabled workspace identities
// for authenticated user pickers. A search term matches the same full name and
// email labels presented by the UI; results are always scoped to the routed
// tenant. This is deliberately separate from the administrative user list.
// GET /api/system/user-directory?q=<name-or-email>&limit=<1..50>
func (h *Handler) HandleUserDirectory(c *gin.Context) {
	if _, verified := requestExecution(c); !verified {
		c.JSON(http.StatusUnauthorized, ErrorResponse{
			Error: ErrorBody{Code: "auth.unauthorized", Message: "Authentication required"},
		})
		return
	}

	db := auth.SiteDB(c)
	if db == nil {
		writeError(c, http.StatusInternalServerError, "server.database_unavailable", "No database connection", nil)
		return
	}

	search := strings.TrimSpace(c.Query("q"))
	requestedNames := strings.TrimSpace(c.Query("names"))
	if search != "" && requestedNames != "" {
		badRequestError(c, "validation.invalid_query", "Use either a search term or a set of user names", nil)
		return
	}
	if len(search) > 120 {
		badRequestError(c, "validation.invalid_query", "Search term is too long", nil)
		return
	}
	limit := 25
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 50 {
			badRequestError(c, "validation.invalid_limit", "Limit must be between 1 and 50", nil)
			return
		}
		limit = parsed
	}

	query := "SELECT name, email, full_name FROM _kora_user WHERE site = ?"
	args := []any{siteName(c)}
	if requestedNames != "" {
		names := strings.Split(requestedNames, ",")
		if len(names) > 100 {
			badRequestError(c, "validation.too_many_names", "At most 100 user names may be resolved at once", nil)
			return
		}
		placeholders := make([]string, 0, len(names))
		seen := make(map[string]struct{}, len(names))
		args = []any{siteName(c)}
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" || len(name) > 128 {
				badRequestError(c, "validation.invalid_user_name", "User names must be non-empty and at most 128 characters", nil)
				return
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			placeholders = append(placeholders, "?")
			args = append(args, name)
		}
		// A disabled account may still be referenced by historical business
		// records. Resolve its label for display, while keeping it out of the
		// interactive picker search below.
		query += " AND name IN (" + strings.Join(placeholders, ", ") + ") ORDER BY full_name, name"
	} else if search != "" {
		query += " AND enabled = ?"
		args = append(args, kdb.BoolParam(h.siteDialect(c), true))
		// The picker searches the same visible fields, but uses a literal
		// prefix so the tenant-scoped indexes declared for each dialect can
		// serve the query. Substring scans become expensive as a workspace's
		// user directory grows.
		pattern := escapeLikePattern(search) + "%"
		if h.siteDialect(c).DriverName() == "postgres" {
			pattern = strings.ToLower(pattern)
			query += " AND (LOWER(full_name) LIKE ? ESCAPE '!' OR LOWER(email) LIKE ? ESCAPE '!')"
		} else {
			query += " AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!')"
		}
		args = append(args, pattern, pattern)
		query += " ORDER BY full_name, name LIMIT ?"
		args = append(args, limit)
	} else {
		query += " AND enabled = ?"
		args = append(args, kdb.BoolParam(h.siteDialect(c), true))
		query += " ORDER BY full_name, name LIMIT ?"
		args = append(args, limit)
	}

	rows, err := db.Query(h.siteQuery(c, query), args...)
	if err != nil {
		internalError(c, "user directory query failed", err)
		return
	}
	defer rows.Close()

	users := make([]UserDirectoryEntry, 0, limit)
	for rows.Next() {
		var user UserDirectoryEntry
		if err := rows.Scan(&user.Name, &user.Email, &user.FullName); err != nil {
			internalError(c, "reading user directory", err)
			return
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		internalError(c, "reading user directory", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: users})
}

func escapeLikePattern(value string) string {
	value = strings.ReplaceAll(value, "!", "!!")
	value = strings.ReplaceAll(value, "%", "!%")
	return strings.ReplaceAll(value, "_", "!_")
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

	var req UserAccessUpdate
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request: "+err.Error(), nil)
		return
	}

	var assignments []string
	var args []any
	// Only tenant-scoped permissions and local access state are mutable here.
	if req.Roles != nil {
		rolesStr := strings.Join(req.Roles, ",")
		assignments = append(assignments, "roles = ?")
		args = append(args, rolesStr)
	}
	if req.Enabled != nil {
		assignments = append(assignments, "enabled = ?")
		args = append(args, *req.Enabled)
	}
	if len(assignments) > 0 {
		tx, err := db.BeginTx(c.Request.Context(), nil)
		if err != nil {
			internalError(c, "updating user", err)
			return
		}
		defer tx.Rollback()
		assignments = append(assignments, "modified = CURRENT_TIMESTAMP")
		args = append(args, site, name)
		query := "UPDATE _kora_user SET " + strings.Join(assignments, ", ") + " WHERE site = ? AND name = ?"
		result, err := tx.Exec(h.siteQuery(c, query), args...)
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
		if req.Roles != nil || req.Enabled != nil {
			if err := configstore.BumpAuthorizationRevisionTx(tx, h.siteDialect(c), false, true); err != nil {
				internalError(c, "updating membership revision", err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			internalError(c, "committing user update", err)
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

// Helpers
// ---------------------------------------------------------------------------

// requireAdmin checks if the current user has the admin role.
// Returns false and writes 403 if not an admin.
func requireAdmin(c *gin.Context) bool {
	execution, verified := requestExecution(c)
	if !verified {
		c.JSON(http.StatusForbidden, ErrorResponse{
			Error: ErrorBody{Code: "permission.admin_required", Message: "Administrator role required"},
		})
		return false
	}
	roles := execution.Actor().Roles
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
