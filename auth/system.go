package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// consoleSessionLifetime is how long a console session lasts.
const consoleSessionLifetime = 24 * time.Hour

// consoleSessionCleanupInterval is how often expired sessions are purged.
const consoleSessionCleanupInterval = 10 * time.Minute

// ConsoleSession represents an authenticated console session stored server-side.
type ConsoleSession struct {
	Email     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SystemCredentials holds the system admin credential loaded from YAML.
type SystemCredentials struct {
	SystemAdmin struct {
		Email    string `yaml:"email"`
		Password string `yaml:"password"` // plaintext — hashed on startup
	} `yaml:"system_admin"`
}

// SystemGuard authenticates system administrator requests for /console.
type SystemGuard struct {
	email        string
	passwordHash string // bcrypt hash, computed on startup
	isDefault    bool   // true if using baked-in default password (forces change)

	// In-memory session store for console sessions.
	mu       sync.RWMutex
	sessions map[string]*ConsoleSession
}

// LoadSystemGuard reads system_credentials.yaml, hashes the password, and returns a guard.
func LoadSystemGuard(path string) (*SystemGuard, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading system credentials: %w (create system_credentials.yaml)", err)
	}

	var creds SystemCredentials
	if err := yaml.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parsing system credentials: %w", err)
	}

	if creds.SystemAdmin.Email == "" || creds.SystemAdmin.Password == "" {
		return nil, fmt.Errorf("system_credentials.yaml must have system_admin.email and system_admin.password")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(creds.SystemAdmin.Password), bcrypt.DefaultCost+2) // cost 12 for admin
	if err != nil {
		return nil, fmt.Errorf("hashing system password: %w", err)
	}

	guard := &SystemGuard{
		email:        creds.SystemAdmin.Email,
		passwordHash: string(hash),
		sessions:     make(map[string]*ConsoleSession),
	}

	// Start background cleanup of expired sessions.
	go guard.cleanupLoop()

	slog.Info("system guard loaded", "email", creds.SystemAdmin.Email)
	return guard, nil
}

// CreateSession creates a new console session for the given email.
// Returns a cryptographically random session ID.
func (g *SystemGuard) CreateSession(email string) string {
	g.mu.Lock()
	defer g.mu.Unlock()

	sid := generateConsoleSessionID()
	now := time.Now()
	g.sessions[sid] = &ConsoleSession{
		Email:     email,
		CreatedAt: now,
		ExpiresAt: now.Add(consoleSessionLifetime),
	}
	return sid
}

// ValidateSession checks whether a session ID is valid and returns the associated email.
// Returns empty string if the session is invalid or expired.
func (g *SystemGuard) ValidateSession(sid string) string {
	g.mu.RLock()
	session, ok := g.sessions[sid]
	g.mu.RUnlock()

	if !ok {
		return ""
	}

	if time.Now().After(session.ExpiresAt) {
		g.DeleteSession(sid)
		return ""
	}

	return session.Email
}

// DeleteSession removes a console session.
func (g *SystemGuard) DeleteSession(sid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.sessions, sid)
}

// cleanupLoop periodically removes expired sessions.
func (g *SystemGuard) cleanupLoop() {
	ticker := time.NewTicker(consoleSessionCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		g.cleanupExpired()
	}
}

func (g *SystemGuard) cleanupExpired() {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now()
	for sid, session := range g.sessions {
		if now.After(session.ExpiresAt) {
			delete(g.sessions, sid)
		}
	}
}

// Middleware returns a Gin middleware that enforces system admin authentication.
// It accepts two authentication methods:
//  1. Cookie: "kora_console_sid" with a server-validated session ID.
//  2. Authorization header: "Basic <base64(email:password)>" for API clients.
func (g *SystemGuard) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip login page and POST.
		if c.Request.URL.Path == "/console/login" {
			c.Next()
			return
		}

		// 1. Check Authorization header (API clients).
		email, ok := g.checkAuthHeader(c)
		if ok {
			c.Set("is_system_admin", true)
			c.Set("system_email", email)
			c.Next()
			return
		}

		// 2. Check session cookie (browser).
		sid, err := c.Cookie("kora_console_sid")
		if err == nil && sid != "" {
			email := g.ValidateSession(sid)
			if email != "" {
				c.Set("is_system_admin", true)
				c.Set("system_email", email)
				c.Next()
				return
			}
			// Session invalid — clear the cookie.
			SetSecureCookie(c, "kora_console_sid", "", -1, "/", true)
		}

		// Browser requests get a redirect to the login page; API clients get 401 JSON.
		if strings.Contains(c.GetHeader("Accept"), "text/html") {
			c.Redirect(http.StatusFound, "/console/login")
			c.Abort()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "system authentication required"})
	}
}

// ValidateWithChangeCheck validates credentials and returns whether a password change is required.
// Password change is required when using the baked-in default credentials.
func (g *SystemGuard) ValidateWithChangeCheck(email, password string) (valid bool, needsChange bool) {
	if email != g.email {
		return false, false
	}
	if err := bcrypt.CompareHashAndPassword([]byte(g.passwordHash), []byte(password)); err != nil {
		return false, false
	}
	return true, g.isDefault
}

// ValidateSessionBool returns true if the session token is valid.
func (g *SystemGuard) ValidateSessionBool(sid string) bool {
	return g.ValidateSession(sid) != ""
}

// UpdatePassword replaces the stored password hash. Used for first-login forced change.
func (g *SystemGuard) UpdatePassword(newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost+2)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.passwordHash = string(hash)
	g.isDefault = false
	return nil
}

// LoadSystemGuardFromEnv creates a SystemGuard from environment variables.
// Falls back to baked-in defaults if env vars are not set.
// Default: admin@kora.local / kora123 (isDefault=true forces password change).
func LoadSystemGuardFromEnv() *SystemGuard {
	email := os.Getenv("CONSOLE_EMAIL")
	password := os.Getenv("CONSOLE_PASSWORD")
	isDefault := false

	if email == "" {
		email = "admin@kora.local"
	}
	if password == "" {
		password = "kora123"
		isDefault = true
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost+2)
	if err != nil {
		slog.Error("failed to hash console password", "error", err)
		return nil
	}

	guard := &SystemGuard{
		email:        email,
		passwordHash: string(hash),
		isDefault:    isDefault,
		sessions:     make(map[string]*ConsoleSession),
	}

	go guard.cleanupLoop()

	slog.Info("system guard loaded from env", "email", email, "is_default", isDefault)
	return guard
}

// checkAuthHeader validates the Authorization header.
// Supports:
//   - "Basic <base64(email:password)>" — HTTP Basic auth (RFC 7617)
//   - "Bearer <console_session_id>" — console session token
func (g *SystemGuard) checkAuthHeader(c *gin.Context) (string, bool) {
	authHeader := c.GetHeader("Authorization")

	// Basic auth (base64-encoded email:password).
	if len(authHeader) > 6 && authHeader[:6] == "Basic " {
		encoded := authHeader[6:]
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", false
		}
		email, password, ok := parseBasicAuth(string(decoded))
		if !ok {
			return "", false
		}
		if email == g.email && bcrypt.CompareHashAndPassword([]byte(g.passwordHash), []byte(password)) == nil {
			return email, true
		}
		return "", false
	}

	// Bearer token — must be a valid console session ID.
	if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
		token := authHeader[7:]
		if email := g.ValidateSession(token); email != "" {
			return email, true
		}
	}

	return "", false
}

// VerifyCredentials checks if the given email and password match the system credentials.
func (g *SystemGuard) VerifyCredentials(email, password string) error {
	if email != g.email {
		return fmt.Errorf("invalid credentials")
	}
	return bcrypt.CompareHashAndPassword([]byte(g.passwordHash), []byte(password))
}

// parseBasicAuth splits a string of the form "email:password" into its parts.
func parseBasicAuth(token string) (string, string, bool) {
	for i := 0; i < len(token); i++ {
		if token[i] == ':' {
			return token[:i], token[i+1:], true
		}
	}
	return "", "", false
}

// generateConsoleSessionID returns a cryptographically random 64-character hex string.
func generateConsoleSessionID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
