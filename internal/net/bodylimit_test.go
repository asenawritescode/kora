package net

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRequestBodyLimitByRoute(t *testing.T) {
	tests := []struct {
		name  string
		route string
		env   map[string]string
		want  int64
	}{
		{name: "default route cap is 10 MiB", route: "/api/v1/resource/Sale", want: 10 << 20},
		{name: "nested upload path gets the default cap", route: "/api/v1/resource/X/upload", want: 10 << 20},
		{name: "request cap is configurable", route: "/api/v1/resource/Sale", env: map[string]string{"KORA_MAX_REQUEST_MB": "2"}, want: 2 << 20},
		{name: "upload route uses the upload cap", route: "/api/v1/upload", want: 50 << 20},
		{name: "legacy upload route uses the upload cap", route: "/api/upload", env: map[string]string{"KORA_MAX_UPLOAD_MB": "7"}, want: 7 << 20},
		{name: "offline intents keep the handler cap", route: "/api/v2/offline/intents", want: offlineIntentBodyLimit},
		{name: "invalid request cap falls back to default", route: "/api/v1/x", env: map[string]string{"KORA_MAX_REQUEST_MB": "nope"}, want: 10 << 20},
		{name: "request cap is bounded to 1 GiB", route: "/api/v1/x", env: map[string]string{"KORA_MAX_REQUEST_MB": "99999"}, want: 1024 << 20},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KORA_MAX_REQUEST_MB", "")
			t.Setenv("KORA_MAX_UPLOAD_MB", "")
			for name, value := range test.env {
				t.Setenv(name, value)
			}
			if got := requestBodyLimit(test.route); got != test.want {
				t.Errorf("requestBodyLimit(%q) = %d, want %d", test.route, got, test.want)
			}
		})
	}
}

func TestRouteTemplateStripsSitePrefixForPathRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var got string
	r.NoRoute(func(c *gin.Context) { got = routeTemplate(c) })
	for path, want := range map[string]string{
		"/s/acme/api/v1/upload": "/api/v1/upload",
		"/s/acme":               "/s/acme",
		"/api/v1/x/upload":      "/api/v1/x/upload",
	} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		if got != want {
			t.Errorf("routeTemplate(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestRequestBodyReachesHandlerAndOverflowBecomes413(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_MAX_REQUEST_MB", "1")
	r := gin.New()
	r.Use(RequestBodyLimitMiddleware())
	r.POST("/api/v1/echo", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable"})
			return
		}
		c.String(http.StatusOK, string(body))
	})

	small := httptest.NewRecorder()
	r.ServeHTTP(small, httptest.NewRequest(http.MethodPost, "/api/v1/echo", strings.NewReader("hello")))
	if small.Code != http.StatusOK || small.Body.String() != "hello" {
		t.Fatalf("small body: status %d body %q, want 200 hello", small.Code, small.Body.String())
	}

	chunked := httptest.NewRequest(http.MethodPost, "/api/v1/echo", io.LimitReader(zeroReader{}, 2<<20))
	chunked.ContentLength = -1
	overflow := httptest.NewRecorder()
	r.ServeHTTP(overflow, chunked)
	if overflow.Code != http.StatusRequestEntityTooLarge || !strings.Contains(overflow.Body.String(), "request.body_too_large") {
		t.Fatalf("chunked overflow: status %d body %q, want 413 envelope", overflow.Code, overflow.Body.String())
	}
}

func TestSiteRedispatchKeepsBodyAndCapsOverflow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_MAX_REQUEST_MB", "1")
	r := gin.New()
	r.Use(RequestBodyLimitMiddleware())
	r.POST("/api/v1/echo", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable"})
			return
		}
		c.String(http.StatusOK, string(body))
	})
	r.NoRoute(func(c *gin.Context) {
		c.Request.URL.Path = "/api/v1/echo"
		r.HandleContext(c)
	})

	ok := httptest.NewRecorder()
	r.ServeHTTP(ok, httptest.NewRequest(http.MethodPost, "/s/acme/api/v1/echo", strings.NewReader("site body")))
	if ok.Code != http.StatusOK || ok.Body.String() != "site body" {
		t.Fatalf("re-dispatched body: status %d body %q, want 200 site body", ok.Code, ok.Body.String())
	}

	chunked := httptest.NewRequest(http.MethodPost, "/s/acme/api/v1/echo", io.LimitReader(zeroReader{}, 2<<20))
	chunked.ContentLength = -1
	overflow := httptest.NewRecorder()
	r.ServeHTTP(overflow, chunked)
	if overflow.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("re-dispatched overflow: status %d, want 413", overflow.Code)
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

func TestOverflowEnvelopeIsPlainUnderGzipOnDirectAndSitePaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_MAX_REQUEST_MB", "1")
	tests := []struct {
		name     string
		path     string
		bodySize int
	}{
		{name: "direct small body", path: "/api/v1/echo", bodySize: 64},
		{name: "direct large body", path: "/api/v1/echo", bodySize: 64 << 10},
		{name: "site small body", path: "/s/acme/api/v1/echo", bodySize: 64},
		{name: "site large body", path: "/s/acme/api/v1/echo", bodySize: 64 << 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := gin.New()
			r.Use(RequestBodyLimitMiddleware())
			group := r.Group("/api/v1")
			group.Use(CompressMiddleware())
			group.POST("/echo", func(c *gin.Context) {
				_, _ = io.ReadAll(c.Request.Body)
				c.Data(http.StatusOK, "text/plain; charset=utf-8", bytes.Repeat([]byte("x"), test.bodySize))
			})
			r.NoRoute(func(c *gin.Context) {
				c.Request.URL.Path = "/api/v1/echo"
				r.HandleContext(c)
			})

			request := httptest.NewRequest(http.MethodPost, test.path, io.LimitReader(zeroReader{}, 2<<20))
			request.ContentLength = -1
			request.Header.Set("Accept-Encoding", "gzip")
			recorder := httptest.NewRecorder()
			r.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", recorder.Code)
			}
			if enc := recorder.Header().Get("Content-Encoding"); enc != "" {
				t.Fatalf("Content-Encoding = %q on a plain 413 envelope", enc)
			}
			body := recorder.Body.Bytes()
			if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
				t.Fatalf("Content-Length = %q, want %d", got, len(body))
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error.Code != "request.body_too_large" {
				t.Fatalf("body is not the 413 envelope: %v %q", err, body)
			}
		})
	}
}

func TestWrappersKeepWriteDeadlineControl(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestBodyLimitMiddleware())
	group := r.Group("/api/v1")
	group.Use(CompressMiddleware())
	deadlineErr := make(chan error, 1)
	group.GET("/stream", func(c *gin.Context) {
		deadlineErr <- http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(time.Minute))
		c.Status(http.StatusOK)
	})
	server := httptest.NewServer(r)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept-Encoding", "gzip")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if err := <-deadlineErr; err != nil {
		t.Fatalf("SetWriteDeadline through the response wrappers: %v", err)
	}
}
