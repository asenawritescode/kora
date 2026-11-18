package net

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// requestBodyLimit returns the byte cap for a request body, given the route
// template or rewritten path. Only exact routes get their larger caps: upload
// routes use KORA_MAX_UPLOAD_MB, and offline intent batches keep the cap their
// handler enforces. Every other route uses KORA_MAX_REQUEST_MB.
func requestBodyLimit(route string) int64 {
	switch route {
	case "/api/upload", "/api/v1/upload":
		return envMegabytes("KORA_MAX_UPLOAD_MB", defaultMaxUploadMB)
	case "/api/v2/offline/intents":
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

// routeTemplate is the route the request matched. Path-routed requests
// (/s/:site/...) match no template, so the site prefix is stripped instead.
func routeTemplate(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	path := c.Request.URL.Path
	if rest, ok := strings.CutPrefix(path, "/s/"); ok {
		if _, after, found := strings.Cut(rest, "/"); found {
			return "/" + after
		}
	}
	return path
}

// bodyLimitState records that a body read exceeded the cap.
type bodyLimitState struct {
	overflowed bool
	replaced   bool
}

// limitedBody reads through http.MaxBytesReader and notes when the cap is hit.
type limitedBody struct {
	io.ReadCloser
	state *bodyLimitState
}

func (b limitedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		b.state.overflowed = true
	}
	return n, err
}

// limitedWriter turns a response into the 413 envelope once a body read has
// overflowed, whatever status or body the handler tries to write.
type limitedWriter struct {
	gin.ResponseWriter
	state *bodyLimitState
}

func (w *limitedWriter) WriteHeader(code int) {
	if w.state.overflowed {
		code = http.StatusRequestEntityTooLarge
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *limitedWriter) WriteHeaderNow() {
	if w.state.overflowed {
		w.writeEnvelope()
		return
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if w.state.overflowed {
		w.writeEnvelope()
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *limitedWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *limitedWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// writeEnvelope sends the 413 envelope once. Headers the handler set for its
// own body are replaced: a stale Content-Encoding or Content-Length would
// describe a body that is no longer sent.
func (w *limitedWriter) writeEnvelope() {
	if w.state.replaced {
		return
	}
	w.state.replaced = true
	body, _ := json.Marshal(gin.H{"error": gin.H{"code": "request.body_too_large", "message": "Request body exceeds the size limit."}})
	h := w.Header()
	h.Del("Content-Encoding")
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.ResponseWriter.WriteHeader(http.StatusRequestEntityTooLarge)
	_, _ = w.ResponseWriter.Write(body)
}

// bodyLimitStateKey carries the request's cap state across site re-dispatch,
// which resets the gin context but keeps the request.
type bodyLimitStateKey struct{}

func bodyLimitStateFrom(c *gin.Context) (*bodyLimitState, bool) {
	state, ok := c.Request.Context().Value(bodyLimitStateKey{}).(*bodyLimitState)
	return state, ok
}

// RequestBodyLimitMiddleware caps request bodies. A declared length over the
// cap is refused with 413 before the handler runs. A body that is larger than
// declared, or chunked, is cut off during the read, and the response becomes
// 413 whatever the handler writes. Site re-dispatch re-runs this middleware on
// the same request: the body cap is installed once and the response writer is
// wrapped again, since re-dispatch resets it.
func RequestBodyLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		state, redispatched := bodyLimitStateFrom(c)
		if !redispatched {
			limit := requestBodyLimit(routeTemplate(c))
			if c.Request.ContentLength > limit {
				abortBodyTooLarge(c)
				return
			}
			state = &bodyLimitState{}
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), bodyLimitStateKey{}, state))
			if c.Request.Body != nil {
				c.Request.Body = limitedBody{ReadCloser: http.MaxBytesReader(c.Writer, c.Request.Body, limit), state: state}
			}
		}
		lw := &limitedWriter{ResponseWriter: c.Writer, state: state}
		c.Writer = lw
		c.Next()
		if state.overflowed {
			lw.writeEnvelope()
		}
	}
}

func abortBodyTooLarge(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
		"error": gin.H{"code": "request.body_too_large", "message": "Request body exceeds the size limit."},
	})
}
