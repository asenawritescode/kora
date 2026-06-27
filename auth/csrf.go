package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"
)

// CSRFSecure controls the Secure flag on CSRF cookies. Set true in production with TLS.
// Deprecated: SetSecureCookie auto-detects TLS; this is kept for explicit config overrides.
var CSRFSecure = false

// SetCSRFSecure sets the Secure flag for CSRF cookies.
func SetCSRFSecure(secure bool) { CSRFSecure = secure }

// SetSecureCookie sets a cookie with Secure auto-detected from the request's TLS state
// and appends SameSite=Lax. Use this instead of c.SetCookie for security-sensitive cookies.
func SetSecureCookie(c *gin.Context, name, value string, maxAge int, path string, httpOnly bool) {
	secure := c.Request.TLS != nil
	c.SetCookie(name, value, maxAge, path, "", secure, httpOnly)
	// Gin's SetCookie doesn't support SameSite; append it manually.
	appendSameSite(c, "Lax")
}

// appendSameSite appends a SameSite attribute to the most recently set cookie.
func appendSameSite(c *gin.Context, sameSite string) {
	headers := c.Writer.Header()["Set-Cookie"]
	if len(headers) == 0 {
		return
	}
	last := headers[len(headers)-1]
	if !stringsContains(last, "SameSite") {
		headers[len(headers)-1] = last + "; SameSite=" + sameSite
	}
}

func stringsContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// CSRFMiddleware protects state-changing requests with a double-submit cookie pattern.
// On first GET, a CSRF token is set as a cookie (HttpOnly=false so JS can read it).
// On POST/PUT/DELETE, the X-Kora-CSRF-Token header must match the cookie value.
// Token comparison uses constant-time comparison to prevent timing attacks.
func CSRFMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip safe methods.
		if c.Request.Method == "GET" || c.Request.Method == "HEAD" || c.Request.Method == "OPTIONS" {
			ensureCSRFCookie(c)
			c.Next()
			return
		}

		// Skip CSRF for API clients using Bearer tokens — the Authorization header
		// cannot be set cross-origin, so CSRF is not a concern for these requests.
		if c.GetHeader("Authorization") != "" {
			c.Next()
			return
		}

		// For state-changing methods, verify the token.
		cookieToken, err := c.Cookie("kora_csrf")
		if err != nil || cookieToken == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "csrf_token_required",
				"message": "CSRF token is required for state-changing requests.",
			})
			return
		}

		headerToken := c.GetHeader("X-Kora-CSRF-Token")
		if headerToken == "" {
			// Also check X-CSRF-Token for compatibility.
			headerToken = c.GetHeader("X-CSRF-Token")
		}

		// Constant-time comparison to prevent timing attacks.
		if subtle.ConstantTimeCompare([]byte(headerToken), []byte(cookieToken)) != 1 {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   "csrf_token_mismatch",
				"message": "CSRF token mismatch.",
			})
			return
		}

		c.Next()
	}
}

// ensureCSRFCookie sets a CSRF cookie if one doesn't exist.
func ensureCSRFCookie(c *gin.Context) {
	_, err := c.Cookie("kora_csrf")
	if err == nil {
		return // Already exists.
	}

	token := generateCSRFToken()
	SetSecureCookie(c, "kora_csrf", token, 86400, "/", false)
	c.Set("csrf_token", token)
}

func generateCSRFToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
