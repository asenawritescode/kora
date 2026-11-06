package net

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func compressTestServer(t *testing.T, handler gin.HandlerFunc) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CompressMiddleware())
	r.Any("/x", handler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// doRaw sends a request without the transport's transparent gzip handling so
// the test sees exactly what is on the wire. A body shorter than the declared
// Content-Length surfaces as a read error, the browser-visible symptom of #333.
func doRaw(t *testing.T, method, url string, hdr map[string]string) (*http.Response, []byte, error) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func TestCompressMiddleware(t *testing.T) {
	large := strings.Repeat("<tr><td>receipt line</td></tr>\n", 200)
	small := "<p>331 bytes receipt</p>"
	tests := []struct {
		name       string
		method     string
		hdr        map[string]string
		handler    gin.HandlerFunc
		wantStatus int
		wantGzip   bool
		wantBody   string // expected (decoded) body; empty for HEAD/204
	}{
		{
			name:       "print-like c.Data with explicit Content-Length is gzipped without stale length",
			method:     http.MethodGet,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.Data(200, "text/html; charset=utf-8", []byte(large)) },
			wantStatus: 200, wantGzip: true, wantBody: large,
		},
		{
			name:       "large body without Content-Length is gzipped",
			method:     http.MethodGet,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.String(200, "%s", large) },
			wantStatus: 200, wantGzip: true, wantBody: large,
		},
		{
			name:       "small body with Content-Length is left plain",
			method:     http.MethodGet,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.Data(200, "text/html", []byte(small)) },
			wantStatus: 200, wantBody: small,
		},
		{
			name:       "small body without Content-Length is left plain",
			method:     http.MethodGet,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.String(200, "%s", small) },
			wantStatus: 200, wantBody: small,
		},
		{
			name:       "client without gzip gets plain body with correct length",
			method:     http.MethodGet,
			handler:    func(c *gin.Context) { c.Data(200, "text/html", []byte(large)) },
			wantStatus: 200, wantBody: large,
		},
		{
			name:   "range request is untouched",
			method: http.MethodGet,
			hdr:    map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9"},
			handler: func(c *gin.Context) {
				c.Header("Content-Range", "bytes 0-9/"+strconv.Itoa(len(large)))
				c.Header("Content-Length", "10")
				c.Status(http.StatusPartialContent)
				_, _ = c.Writer.Write([]byte(large[:10]))
			},
			wantStatus: 206, wantBody: large[:10],
		},
		{
			name:       "HEAD keeps the handler Content-Length and is not encoded",
			method:     http.MethodHead,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.Data(200, "text/html", []byte(large)) },
			wantStatus: 200,
		},
		{
			name:   "already encoded response is not double-compressed",
			method: http.MethodGet,
			hdr:    map[string]string{"Accept-Encoding": "gzip"},
			handler: func(c *gin.Context) {
				c.Header("Content-Encoding", "br")
				c.Data(200, "text/html", []byte(large))
			},
			wantStatus: 200, wantBody: large,
		},
		{
			name:       "already compressed media type is not compressed",
			method:     http.MethodGet,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.Data(200, "application/pdf", []byte(large)) },
			wantStatus: 200, wantBody: large,
		},
		{
			name:       "no content is not encoded",
			method:     http.MethodGet,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.Status(http.StatusNoContent) },
			wantStatus: 204,
		},
		{
			name:       "bodyless error is not encoded",
			method:     http.MethodGet,
			hdr:        map[string]string{"Accept-Encoding": "gzip"},
			handler:    func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) },
			wantStatus: 401,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := compressTestServer(t, tt.handler)
			resp, body, err := doRaw(t, tt.method, srv.URL+"/x", tt.hdr)
			if err != nil {
				t.Fatalf("reading body: %v (declared Content-Length %q)", err, resp.Header.Get("Content-Length"))
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
				t.Errorf("Vary = %q, want Accept-Encoding", resp.Header.Get("Vary"))
			}
			enc := resp.Header.Get("Content-Encoding")
			if tt.wantGzip != (enc == "gzip") {
				t.Fatalf("Content-Encoding = %q, wantGzip = %v", enc, tt.wantGzip)
			}
			if tt.method == http.MethodHead {
				if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(large)) {
					t.Errorf("HEAD Content-Length = %q, want %d", got, len(large))
				}
				return
			}
			if cl := resp.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(body)) {
				t.Errorf("Content-Length %s does not match %d body bytes", cl, len(body))
			}
			if tt.wantGzip {
				body = gunzip(t, body)
			}
			if string(body) != tt.wantBody {
				t.Errorf("decoded body mismatch: got %d bytes, want %d", len(body), len(tt.wantBody))
			}
		})
	}
}

func TestCompressMiddlewareStreamsEventStream(t *testing.T) {
	release := make(chan struct{})
	srv := compressTestServer(t, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: one\n\n")
		c.Writer.Flush()
		<-release
	})
	defer close(release)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("event stream Content-Encoding = %q, want none", enc)
	}
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if s != "data: one\n" {
			t.Fatalf("first line = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event was buffered instead of flushed")
	}
}
