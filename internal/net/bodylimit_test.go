package net

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestBodyLimitByRoute(t *testing.T) {
	t.Setenv("KORA_MAX_REQUEST_MB", "")
	t.Setenv("KORA_MAX_UPLOAD_MB", "")
	tests := []struct {
		name string
		path string
		env  map[string]string
		want int64
	}{
		{name: "default route cap is 10 MiB", path: "/api/v1/resource/Sale", want: 10 << 20},
		{name: "request cap is configurable", path: "/api/v1/resource/Sale", env: map[string]string{"KORA_MAX_REQUEST_MB": "2"}, want: 2 << 20},
		{name: "upload route uses the upload cap", path: "/api/v1/upload", want: 50 << 20},
		{name: "path-routed upload uses the upload cap", path: "/s/acme/api/v1/upload", env: map[string]string{"KORA_MAX_UPLOAD_MB": "7"}, want: 7 << 20},
		{name: "offline intents keep the handler cap", path: "/api/v2/offline/intents", want: offlineIntentBodyLimit},
		{name: "invalid request cap falls back to default", path: "/api/v1/x", env: map[string]string{"KORA_MAX_REQUEST_MB": "nope"}, want: 10 << 20},
		{name: "request cap is bounded to 1 GiB", path: "/api/v1/x", env: map[string]string{"KORA_MAX_REQUEST_MB": "99999"}, want: 1024 << 20},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			if got := requestBodyLimit(test.path); got != test.want {
				t.Errorf("requestBodyLimit(%q) = %d, want %d", test.path, got, test.want)
			}
		})
	}
}

func TestRequestBodyLimitMiddlewareRefusesOversizedDeclaredBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_MAX_REQUEST_MB", "1")
	r := gin.New()
	r.Use(RequestBodyLimitMiddleware())
	reached := false
	r.POST("/api/v1/x", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/x", strings.NewReader(strings.Repeat("a", 2<<20)))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || reached {
		t.Fatalf("oversized body: status %d reached=%v, want 413 and handler not reached", w.Code, reached)
	}
	if !strings.Contains(w.Body.String(), "request.body_too_large") {
		t.Errorf("413 body lacks the error envelope: %s", w.Body.String())
	}
}

func TestRequestBodyLimitMiddlewareCutsOffUnboundedRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_MAX_REQUEST_MB", "1")
	r := gin.New()
	r.Use(RequestBodyLimitMiddleware())
	var readErr error
	r.POST("/api/v1/x", func(c *gin.Context) {
		_, readErr = io.ReadAll(c.Request.Body)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/x", io.LimitReader(zeroReader{}, 2<<20))
	req.ContentLength = -1
	r.ServeHTTP(w, req)
	if readErr == nil {
		t.Fatal("body larger than the cap was read to completion")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestRateLimiterSetsRetryAfterBeforeRefusal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(NewRateLimiter(0.0001, 1).Middleware())
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	first := httptest.NewRecorder()
	r.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/x", nil))
	second := httptest.NewRecorder()
	r.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/x", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", second.Code)
	}
	if got := second.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
}

func TestServerHeaderLimits(t *testing.T) {
	srv := NewServer(http.NotFoundHandler(), "127.0.0.1:0", nil)
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.MaxHeaderBytes != 1<<20 {
		t.Errorf("MaxHeaderBytes = %d, want 1 MiB", srv.MaxHeaderBytes)
	}
}
