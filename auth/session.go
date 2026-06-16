package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// User represents an authenticated user.
type User struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	FullName string   `json:"full_name"`
	Roles    []string `json:"roles"`
	Enabled  bool     `json:"enabled"`
}

// SessionLifetime is the duration for which sessions are valid. Set before creating SessionManagers.
var SessionLifetime = 24 * time.Hour

// sessionCacheTTL is how long session lookups are cached before re-validating.
const sessionCacheTTL = 30 * time.Second
const sessionCacheCleanupInterval = 5 * time.Minute

type sessionCacheEntry struct {
	user      *User
	cachedAt  time.Time
	expiresAt time.Time // session expiry from DB
}

// SessionManager manages user sessions with an in-memory TTL cache.
type SessionManager struct {
	DB       *sql.DB
	cacheMu  sync.RWMutex
	cache    map[string]*sessionCacheEntry
}

// NewSessionManager creates a new session manager.
// If db is nil (console-only mode with no sites), the sweep goroutine is not started.
func NewSessionManager(db *sql.DB) *SessionManager {
	sm := &SessionManager{
		DB:    db,
		cache: make(map[string]*sessionCacheEntry),
	}
	if db != nil {
		go sm.sweepCacheLoop()
	}
	return sm
}

// CreateSession creates a new session for a user and returns the session ID.
func (sm *SessionManager) CreateSession(user *User) (string, error) {
	if sm.DB == nil {
		return "", fmt.Errorf("no database connection available")
	}
	sid := generateSessionID()
	expiresAt := time.Now().Add(SessionLifetime)

	// Marshal user data as JSON in Go (dialect-neutral, avoids MySQL-only JSON_OBJECT).
	userData := gin.H{
		"name":      user.Name,
		"email":     user.Email,
		"full_name": user.FullName,
		"roles":     user.Roles,
	}
	userJSON, err := json.Marshal(userData)
	if err != nil {
		return "", fmt.Errorf("marshaling user data: %w", err)
	}

	_, err = sm.DB.Exec(
		`INSERT INTO _kora_session (sid, user, data, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		sid, user.Name, string(userJSON), expiresAt, time.Now(),
	)
	if err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}

	return sid, nil
}

// GetSession validates a session ID and returns the associated user.
// Uses an in-memory TTL cache to avoid hitting the database on every request.
func (sm *SessionManager) GetSession(sid string) (*User, error) {
	if sm.DB == nil {
		return nil, fmt.Errorf("no database connection available")
	}
	// Check cache first.
	sm.cacheMu.RLock()
	entry, ok := sm.cache[sid]
	sm.cacheMu.RUnlock()

	if ok && time.Now().Before(entry.cachedAt.Add(sessionCacheTTL)) {
		if time.Now().After(entry.expiresAt) {
			sm.DeleteSession(sid)
			return nil, fmt.Errorf("session expired")
		}
		return entry.user, nil
	}

	// Cache miss or expired — query database.
	var userJSON string
	var expiresStr string // scanned as string for SQLite compatibility (TEXT column)

	err := sm.DB.QueryRow(
		"SELECT data, expires_at FROM _kora_session WHERE sid = ?",
		sid,
	).Scan(&userJSON, &expiresStr)

	if err == sql.ErrNoRows {
		// Remove from cache if present.
		sm.cacheMu.Lock()
		delete(sm.cache, sid)
		sm.cacheMu.Unlock()
		return nil, fmt.Errorf("session not found")
	}
	if err != nil {
		return nil, fmt.Errorf("querying session: %w", err)
	}

	expiresAt, err := parseTime(expiresStr)
	if err != nil {
		return nil, fmt.Errorf("parsing session expiry: %w", err)
	}

	if time.Now().After(expiresAt) {
		sm.DeleteSession(sid)
		return nil, fmt.Errorf("session expired")
	}

	// Parse JSON. For simplicity in Phase 1, parse manually.
	user := &User{}
	if err := scanUserJSON(userJSON, user); err != nil {
		return nil, fmt.Errorf("parsing session data: %w", err)
	}

	// Populate cache.
	sm.cacheMu.Lock()
	sm.cache[sid] = &sessionCacheEntry{
		user:      user,
		cachedAt:  time.Now(),
		expiresAt: expiresAt,
	}
	sm.cacheMu.Unlock()

	return user, nil
}

func scanUserJSON(jsonStr string, user *User) error {
	// Simple JSON parsing — extract name, email, full_name, roles.
	extract := func(key string) string {
		start := 0
		for {
			idx := indexAfter(jsonStr, `"`+key+`"`, start)
			if idx < 0 {
				return ""
			}
			// Skip whitespace and colon.
			rest := jsonStr[idx:]
			colonIdx := 0
			for colonIdx < len(rest) && (rest[colonIdx] == ' ' || rest[colonIdx] == ':') {
				colonIdx++
			}
			if colonIdx >= len(rest) {
				return ""
			}
			rest = rest[colonIdx:]
			if len(rest) > 0 && rest[0] == '[' {
				// Array value.
				endIdx := 1
				for endIdx < len(rest) && rest[endIdx] != ']' {
					endIdx++
				}
				return rest[:endIdx+1]
			}
			if len(rest) > 0 && rest[0] == '"' {
				// String value.
				endIdx := 1
				for endIdx < len(rest) && rest[endIdx] != '"' {
					endIdx++
				}
				return rest[1:endIdx]
			}
			start = idx + len(key) + 2
		}
	}

	user.Name = extract("name")
	user.Email = extract("email")
	user.FullName = extract("full_name")
	rolesStr := extract("roles")
	if rolesStr != "" {
		// Parse array: ["Role1", "Role2"]
		rolesStr = trim(rolesStr, "[]")
		if rolesStr != "" {
			parts := splitQuoted(rolesStr)
			for _, p := range parts {
				user.Roles = append(user.Roles, p)
			}
		}
	}

	return nil
}

func indexAfter(s, substr string, start int) int {
	for i := start; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i + len(substr)
		}
	}
	return -1
}

func trim(s, cutset string) string {
	for len(s) > 0 && contains(cutset, s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && contains(cutset, s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

func contains(set string, c byte) bool {
	for i := 0; i < len(set); i++ {
		if set[i] == c {
			return true
		}
	}
	return false
}

func splitQuoted(s string) []string {
	var result []string
	var current string
	inQuote := false
	for _, c := range s {
		if c == '"' {
			inQuote = !inQuote
			if !inQuote && current != "" {
				result = append(result, current)
				current = ""
			}
		} else if inQuote {
			current += string(c)
		}
	}
	return result
}

// DeleteSession removes a session.
func (sm *SessionManager) DeleteSession(sid string) {
	if sm.DB == nil {
		return
	}
	_, err := sm.DB.Exec("DELETE FROM _kora_session WHERE sid = ?", sid)
	if err != nil {
		slog.Warn("failed to delete session", "sid", sid, "error", err)
	}
	// Invalidate cache entry.
	sm.cacheMu.Lock()
	delete(sm.cache, sid)
	sm.cacheMu.Unlock()
}

// InvalidateSession removes a session from the cache without deleting it from the database.
// Use this when user state changes (role change, password change, disable) to force re-validation.
func (sm *SessionManager) InvalidateSession(sid string) {
	sm.cacheMu.Lock()
	delete(sm.cache, sid)
	sm.cacheMu.Unlock()
}

// sweepCacheLoop periodically removes expired cache entries and cleans up expired DB sessions.
func (sm *SessionManager) sweepCacheLoop() {
	ticker := time.NewTicker(sessionCacheCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		// Clean in-memory cache.
		sm.cacheMu.Lock()
		now := time.Now()
		for sid, entry := range sm.cache {
			if now.After(entry.expiresAt) || now.After(entry.cachedAt.Add(sessionCacheTTL*2)) {
				delete(sm.cache, sid)
			}
		}
		sm.cacheMu.Unlock()

		// Clean expired DB sessions.
		sm.cleanupExpired()
	}
}

func (sm *SessionManager) cleanupExpired() {
	if sm.DB == nil {
		return // console-only mode — no site database
	}
	_, err := sm.DB.Exec("DELETE FROM _kora_session WHERE expires_at < ?", time.Now())
	if err != nil {
		slog.Warn("failed to cleanup expired sessions", "error", err)
	}
}

// AuthenticateUser verifies a username/email and password against the database.
func (sm *SessionManager) AuthenticateUser(email, password string) (*User, error) {
	if sm.DB == nil {
		return nil, fmt.Errorf("no database connection available")
	}
	var name, emailAddr, passwordHash, fullName, rolesStr string
	var enabled bool

	err := sm.DB.QueryRow(
		"SELECT name, email, password_hash, full_name, enabled, COALESCE(roles, '') FROM _kora_user WHERE email = ?",
		email,
	).Scan(&name, &emailAddr, &passwordHash, &fullName, &enabled, &rolesStr)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("invalid credentials")
	}
	if err != nil {
		return nil, fmt.Errorf("querying user: %w", err)
	}

	if !enabled {
		// Return generic "invalid credentials" to prevent user enumeration.
		// The real reason is logged for audit purposes.
		slog.Warn("login attempt for disabled account", "email", email)
		return nil, fmt.Errorf("invalid credentials")
	}

	// Verify password.
	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}

	// Parse roles.
	var roles []string
	if rolesStr != "" {
		// Simple comma-separated or newline-separated.
		parts := splitRolesStr(rolesStr)
		roles = parts
	}

	return &User{
		Name:     name,
		Email:    emailAddr,
		FullName: fullName,
		Roles:    roles,
		Enabled:  enabled,
	}, nil
}

func splitRolesStr(s string) []string {
	// Try newline first, then comma.
	if containsStr(s, "\n") {
		result := splitStr(s, "\n")
		for i, r := range result {
			result[i] = trimWhitespace(r)
		}
		return result
	}
	result := splitStr(s, ",")
	for i, r := range result {
		result[i] = trimWhitespace(r)
	}
	return result
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && indexAfterStr(s, substr) >= 0
}

func indexAfterStr(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func splitStr(s, sep string) []string {
	var result []string
	for {
		idx := indexAfterStr(s, sep)
		if idx < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	return result
}

func trimWhitespace(s string) string {
	s = trim(s, " \t\r\n")
	return s
}

// HashPassword creates a bcrypt hash of a password.
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func generateSessionID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// AuthMiddleware returns a Gin middleware that validates session cookies.
func AuthMiddleware(sm *SessionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip auth for login endpoint and health check.
		path := c.Request.URL.Path
		if path == "/api/auth/login" || path == "/api/ping" || path == "/workspace/login" || path == "/console/login" {
			c.Next()
			return
		}

		if !validateSession(c, sm) {
			return
		}
		c.Next()
	}
}

// validateSession validates the session cookie/header and sets user context.
// Returns false and writes 401 if auth fails; true if auth succeeded or was skipped.
// Does NOT call c.Next() — callers must do that themselves.
func validateSession(c *gin.Context, sm *SessionManager) bool {
	// Get session cookie.
	sid, err := c.Cookie("kora_sid")
	if err != nil {
		// Try Authorization header.
		authHeader := c.GetHeader("Authorization")
		if stringsHasPrefix(authHeader, "Bearer ") {
			sid = authHeader[7:]
		}
	}

	if sid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		c.Abort()
		return false
	}

	// Use site-specific DB for session validation if available.
	sessionSM := sm
	if siteDB, exists := c.Get("site_db"); exists {
		if sdb, ok := siteDB.(*sql.DB); ok && sdb != sm.DB {
			sessionSM = NewSessionManager(sdb)
		}
	}

	user, err := sessionSM.GetSession(sid)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
		c.Abort()
		return false
	}

	c.Set("user", user.Name)
	c.Set("user_obj", user)

	// Set role info for permission checks.
	if len(user.Roles) > 0 {
		c.Set("user_role", user.Roles[0])
		c.Set("user_roles", user.Roles)
	} else {
		c.Set("user_role", "")
		c.Set("user_roles", []string{})
	}
	return true
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// RegisterAuthRoutes registers authentication endpoints.
func RegisterAuthRoutes(router *gin.Engine, sm *SessionManager, db *sql.DB) {
	auth := router.Group("/api/auth")
	{
		auth.POST("/login", func(c *gin.Context) {
			var req struct {
				Email    string `json:"email"`
				Password string `json:"password"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
				return
			}

			// Use site-specific DB if available (multi-site path-based or Host-based).
			db := db
			if siteDB, exists := c.Get("site_db"); exists {
				if sdb, ok := siteDB.(*sql.DB); ok {
					db = sdb
				}
			}
			sm := NewSessionManager(db)

			user, err := sm.AuthenticateUser(req.Email, req.Password)
			if err != nil {
				// Only return known user-facing messages; log internal errors.
				msg := err.Error()
				if msg != "invalid credentials" {
					slog.Error("login authentication error", "error", err)
					msg = "invalid credentials"
				}
				c.JSON(http.StatusUnauthorized, gin.H{"error": msg})
				return
			}

			sid, err := sm.CreateSession(user)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
				return
			}

			// Set cookie with Secure auto-detected from TLS and SameSite=Lax.
			SetSecureCookie(c, "kora_sid", sid, int(SessionLifetime.Seconds()), "/", true)

			c.JSON(http.StatusOK, gin.H{
				"data": gin.H{
					"name":      user.Name,
					"email":     user.Email,
					"full_name": user.FullName,
					"roles":     user.Roles,
				},
				"sid": sid,
			})
		})

		auth.POST("/logout", func(c *gin.Context) {
			sid, _ := c.Cookie("kora_sid")
			if sid != "" {
				// Use site DB if available.
				logoutSM := sm
				if siteDB, exists := c.Get("site_db"); exists {
					if sdb, ok := siteDB.(*sql.DB); ok { logoutSM = NewSessionManager(sdb) }
				}
				logoutSM.DeleteSession(sid)
			}
			SetSecureCookie(c, "kora_sid", "", -1, "/", true)
			c.JSON(http.StatusOK, gin.H{"message": "logged out"})
		})

		auth.GET("/providers", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{
					"data": gin.H{
						"providers": []gin.H{
							{"name": "password", "label": "Email & Password"},
						},
					},
				})
			})

			auth.GET("/me", func(c *gin.Context) {
			sid, _ := c.Cookie("kora_sid")
			if sid == "" {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "not authenticated"})
				return
			}
			// Use site DB if available.
			meSM := sm
			if siteDB, exists := c.Get("site_db"); exists {
				if sdb, ok := siteDB.(*sql.DB); ok { meSM = NewSessionManager(sdb) }
			}
			user, err := meSM.GetSession(sid)
			if err != nil {
				slog.Warn("session validation failed for /me", "error", err)
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired session"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"data": gin.H{
					"name":      user.Name,
					"email":     user.Email,
					"full_name": user.FullName,
					"roles":     user.Roles,
				},
			})
		})
	}
}

// parseTime parses a datetime string from the database (MySQL DATETIME or SQLite TEXT).
func parseTime(s string) (time.Time, error) {
	// Try RFC 3339 (Go's default time.Time encoding for SQL parameters).
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Try MySQL datetime format with microseconds: "2006-01-02 15:04:05.999999"
	if t, err := time.Parse("2006-01-02 15:04:05.999999", s); err == nil {
		return t, nil
	}
	// Try without microseconds: "2006-01-02 15:04:05"
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}
