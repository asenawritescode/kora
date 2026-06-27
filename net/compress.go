package net

import (
	"compress/gzip"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
)

// gzipResponseWriter wraps gin.ResponseWriter with gzip compression.
type gzipResponseWriter struct {
	gin.ResponseWriter
	writer io.Writer
}

func (g *gzipResponseWriter) Write(data []byte) (int, error) {
	return g.writer.Write(data)
}

func (g *gzipResponseWriter) WriteString(s string) (int, error) {
	return g.writer.Write([]byte(s))
}

// CompressMiddleware returns a Gin middleware that compresses HTTP responses
// with gzip when the client sends Accept-Encoding: gzip.
// The writer is wrapped BEFORE c.Next() so all handler writes go through gzip.
func CompressMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip if client doesn't accept gzip.
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}

		// Skip WebSocket connections.
		if c.GetHeader("Upgrade") != "" {
			c.Next()
			return
		}

		gz := gzip.NewWriter(c.Writer)
		defer gz.Close()

		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")
		c.Writer.Header().Del("Content-Length")

		c.Writer = &gzipResponseWriter{
			ResponseWriter: c.Writer,
			writer:         gz,
		}

		c.Next()
	}
}
