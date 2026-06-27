package workspace

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	knet "github.com/asenawritescode/kora/net"
)

func TestEmbeddedFS_NotEmpty(t *testing.T) {
	sub := SPAFS()
	if sub == nil {
		t.Fatal("SPAFS returned nil")
	}

	// Verify it contains the SPA build output.
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatalf("reading embedded FS root: %v", err)
	}
	if len(entries) == 0 {
		t.Error("embedded FS is empty — SPA may not be built")
	}
}

func TestServesIndexHtml(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sub := SPAFS()
	if sub == nil {
		t.Skip("SPA not built — skipping")
	}
	// Check if SPA is actually built (not just CI's touch-created empty file).
	fi, err := fs.Stat(sub, "index.html")
	if err != nil || fi.Size() < 100 {
		t.Skip("SPA not built (index.html empty or missing) — skipping")
	}

	sr := knet.NewSiteRouter(nil)
	r := gin.New()
	RegisterSPARoutes(r, sr)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/workspace", nil)
	r.ServeHTTP(w, req)

	// The SPA should serve index.html content.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify the content is HTML (from the embedded index.html).
	body := w.Body.String()
	if len(body) < 50 {
		t.Errorf("response body too short (%d bytes); SPA may not be built", len(body))
	}
}

func TestServesStaticAssets(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sr := knet.NewSiteRouter(nil)
	r := gin.New()
	RegisterSPARoutes(r, sr)

	// Request a .js asset directly.
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/workspace/assets/index.js", nil)
	r.ServeHTTP(w, req)

	// If the SPA is built, this serves the file; otherwise it falls through
	// to the embedded handler which may return index.html or the file.
	// We just check it doesn't 500.
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("status = %d, want any non-500", w.Code)
	}
}

func TestNotFoundReturnsIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sr := knet.NewSiteRouter(nil)
	r := gin.New()
	RegisterSPARoutes(r, sr)

	// A non-existent path under /workspace should serve index.html
	// (client-side routing fallback).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/workspace/some/deep/path", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestServesFavicon(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sr := knet.NewSiteRouter(nil)
	r := gin.New()
	RegisterSPARoutes(r, sr)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/favicon.ico", nil)
	r.ServeHTTP(w, req)

	// If no /favicon.ico is in the SPA, the NoRoute handler returns:
	// {"error":"not_found"} with 404. Either response is valid.
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("favicon request returned 500: %s", w.Body.String())
	}
}

func TestStaticHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	sr := knet.NewSiteRouter(nil)
	r := gin.New()
	RegisterSPARoutes(r, sr)

	// Console paths should serve the SPA (index.html fallback).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/console/login", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}
