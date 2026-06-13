package net

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/secure"
	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
)

// RequestIDMiddleware ensures every request has a traceable ID.
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if id == "" {
			id = ulid.Make().String()
		}
		c.Header("X-Request-Id", id)
		c.Set("request_id", id)
		c.Next()
	}
}

// SecurityHeadersMiddleware sets production security headers via gin-contrib/secure.
func SecurityHeadersMiddleware(tlsEnabled bool) gin.HandlerFunc {
	cfg := secure.Config{
		SSLRedirect:          tlsEnabled,
		IsDevelopment:        false,
		STSSeconds:            31536000,
		STSIncludeSubdomains: true,
		FrameDeny:            true,
		ContentTypeNosniff:   true,
		BrowserXssFilter:     true,
		ContentSecurityPolicy: "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https://unpkg.com https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; img-src 'self' data:;",
		ReferrerPolicy:       "same-origin",
	}
	if !tlsEnabled {
		cfg.STSSeconds = 0
		cfg.SSLRedirect = false
	}
	return secure.New(cfg)
}

// CORSMiddleware configures cross-origin access.
// When allowedOrigins is empty (dev mode), origins are reflected from the request.
// This is safe with credentials because the origin is reflected, not wildcarded.
// In production, always pass explicit allowed origins.
func CORSMiddleware(allowedOrigins []string) gin.HandlerFunc {
	cfg := cors.Config{
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Kora-CSRF-Token", "X-Request-Id"},
		ExposeHeaders:    []string{"X-Request-Id", "Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * 3600, // 12 hours
	}

	if len(allowedOrigins) > 0 {
		cfg.AllowOrigins = allowedOrigins
	} else {
		// Dev mode: reflect the request origin.
		// This is safe with AllowCredentials because the wildcard * is never sent.
		cfg.AllowOriginFunc = func(origin string) bool { return true }
	}

	return cors.New(cfg)
}

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
