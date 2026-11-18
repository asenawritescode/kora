package net

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	defaultMaxRequestMB = 10
	defaultMaxUploadMB  = 50
	maxConfiguredMB     = 1024
	// offlineIntentBodyLimit mirrors the cap that the offline intents handler
	// enforces itself (20 intents of 1 MiB plus envelope overhead).
	offlineIntentBodyLimit = 20 * ((1 << 20) + 4096)
)

// requestBodyLimit returns the byte cap for a request body. Upload routes use
// KORA_MAX_UPLOAD_MB, and offline intent batches keep the larger cap their
// handler enforces. Every other route uses KORA_MAX_REQUEST_MB.
func requestBodyLimit(path string) int64 {
	switch {
	case strings.HasSuffix(path, "/upload"):
		return envMegabytes("KORA_MAX_UPLOAD_MB", defaultMaxUploadMB)
	case strings.HasSuffix(path, "/offline/intents"):
		return offlineIntentBodyLimit
	default:
		return envMegabytes("KORA_MAX_REQUEST_MB", defaultMaxRequestMB)
	}
}

func envMegabytes(name string, fallbackMB int64) int64 {
	mb, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil || mb <= 0 {
		mb = fallbackMB
	}
	return min(mb, maxConfiguredMB) << 20
}

// RequestBodyLimitMiddleware caps request bodies. A declared length over the
// cap is refused with 413 before the handler runs; a body that grows past the
// cap is cut off by http.MaxBytesReader during the read.
func RequestBodyLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := requestBodyLimit(c.Request.URL.Path)
		if c.Request.ContentLength > limit {
			abortBodyTooLarge(c)
			return
		}
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}

func abortBodyTooLarge(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
		"error": gin.H{"code": "request.body_too_large", "message": "Request body exceeds the size limit."},
	})
}
