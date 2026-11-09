package net

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// compressMinSize is the smallest body worth gzipping; below it the framing
// overhead and CPU cost outweigh the savings.
const compressMinSize = 1024

// gzipResponseWriter defers the compress/pass-through decision until the
// handler has committed its headers (status, Content-Type, Content-Length),
// because only then is it known whether gzip is valid for the response.
type gzipResponseWriter struct {
	gin.ResponseWriter
	req     *http.Request
	gz      *gzip.Writer
	pending []byte // body held back (under compressMinSize) until a decision
	decided bool
}

// skipCompression reports whether the response must be sent as the handler wrote it.
func (g *gzipResponseWriter) skipCompression() bool {
	h := g.Header()
	switch {
	case g.req.Method == http.MethodHead,
		g.req.Header.Get("Range") != "", h.Get("Content-Range") != "",
		h.Get("Content-Encoding") != "",
		strings.Contains(strings.ToLower(h.Get("Cache-Control")), "no-transform"):
		return true
	}
	switch s := g.Status(); {
	case s < 200, s == http.StatusNoContent, s == http.StatusNotModified, s == http.StatusPartialContent:
		return true
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < compressMinSize {
		return true
	}
	return !compressibleType(h.Get("Content-Type"))
}

// compressibleType is false for event streams (they must flush frame by frame)
// and for formats that are already compressed.
func compressibleType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return true
	}
	switch {
	case strings.HasPrefix(ct, "text/event-stream"),
		strings.HasPrefix(ct, "audio/"), strings.HasPrefix(ct, "video/"),
		strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "image/svg"),
		strings.HasPrefix(ct, "application/pdf"), strings.HasPrefix(ct, "application/zip"),
		strings.HasPrefix(ct, "application/gzip"), strings.HasPrefix(ct, "application/x-gzip"),
		strings.HasPrefix(ct, "application/octet-stream"):
		return false
	}
	return true
}

// start commits to gzip: the declared length no longer describes the body.
func (g *gzipResponseWriter) start() {
	g.decided = true
	h := g.Header()
	if h.Get("Content-Type") == "" && len(g.pending) > 0 {
		h.Set("Content-Type", http.DetectContentType(g.pending))
	}
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")
	g.gz = gzip.NewWriter(g.ResponseWriter)
}

func (g *gzipResponseWriter) Write(data []byte) (int, error) {
	if !g.decided {
		if g.skipCompression() {
			g.decided = true
			if len(g.pending) > 0 {
				held := g.pending
				g.pending = nil
				if _, err := g.ResponseWriter.Write(held); err != nil {
					return 0, err
				}
			}
		} else if _, declared := g.Header()["Content-Length"]; !declared && len(g.pending)+len(data) < compressMinSize {
			// Unknown size: hold small bodies back so they can go out plain.
			g.pending = append(g.pending, data...)
			return len(data), nil
		} else {
			g.pending = append(g.pending, data...)
			g.start()
			_, err := g.gz.Write(g.pending)
			g.pending = nil
			return len(data), err
		}
	}
	if g.gz != nil {
		return g.gz.Write(data)
	}
	return g.ResponseWriter.Write(data)
}

func (g *gzipResponseWriter) WriteString(s string) (int, error) {
	return g.Write([]byte(s))
}

// WriteHeaderNow commits headers early (e.g. AbortWithStatus). With no body
// seen yet the size is unknown, so send the response as written.
func (g *gzipResponseWriter) WriteHeaderNow() {
	if !g.decided && len(g.pending) == 0 {
		g.decided = true
	}
	g.ResponseWriter.WriteHeaderNow()
}

// Flush starts compressing if undecided (a flushing handler is streaming),
// then pushes buffered compressed bytes to the client.
func (g *gzipResponseWriter) Flush() {
	if !g.decided {
		if g.skipCompression() {
			g.decided = true
			g.release()
		} else {
			g.start()
			if len(g.pending) > 0 {
				_, _ = g.gz.Write(g.pending)
				g.pending = nil
			}
		}
	}
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	g.ResponseWriter.Flush()
}

// release writes held-back bytes uncompressed with an exact Content-Length.
func (g *gzipResponseWriter) release() {
	if len(g.pending) == 0 {
		return
	}
	if !g.ResponseWriter.Written() {
		g.Header().Set("Content-Length", strconv.Itoa(len(g.pending)))
	}
	_, _ = g.ResponseWriter.Write(g.pending)
	g.pending = nil
}

// finish completes the response after the handler chain returns.
func (g *gzipResponseWriter) finish() {
	switch {
	case g.gz != nil:
		_ = g.gz.Close()
	case len(g.pending) > 0:
		g.decided = true
		g.release()
	}
}

// CompressMiddleware returns a Gin middleware that gzips HTTP responses when
// the client sends Accept-Encoding: gzip. The compress decision is made lazily
// per response (see gzipResponseWriter) so handlers that set Content-Length,
// answer Range or HEAD requests, stream events, or return tiny or
// already-compressed bodies are never given a mismatched or encoded body.
func CompressMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Responses vary by Accept-Encoding whether or not this one is encoded.
		c.Writer.Header().Add("Vary", "Accept-Encoding")

		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") || c.GetHeader("Upgrade") != "" {
			c.Next()
			return
		}

		gw := &gzipResponseWriter{ResponseWriter: c.Writer, req: c.Request}
		c.Writer = gw
		c.Next()
		gw.finish()
	}
}
