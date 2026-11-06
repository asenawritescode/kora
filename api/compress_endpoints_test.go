package api

import (
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

	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/print"
	"github.com/asenawritescode/kora/storage"
)

// rawGet fetches without transparent gzip handling. A body shorter than the
// declared Content-Length surfaces as a read error (the browser hang in #333).
func rawGet(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
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
	if err != nil {
		t.Fatalf("read body: %v (Content-Length %q, Content-Encoding %q)", err, resp.Header.Get("Content-Length"), resp.Header.Get("Content-Encoding"))
	}
	return resp, body
}

func decodedBody(t *testing.T, resp *http.Response, body []byte) []byte {
	t.Helper()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		return body
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Every print rendering must survive CompressMiddleware: the decompressed body
// equals the plain response and the wire length is never stale.
func TestPrintRoutesUnderGzip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	big := strings.Repeat("line of receipt text ", 400)
	rec, err := print.Load([]byte("name: a\nfor: {record: Sale}\npaper: thermal_58\ntext: \"{{ .doc.title }} " + big + "\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	small, err := print.Load([]byte("name: a\nfor: {record: Sale}\npaper: thermal_58\ntext: \"{{ .doc.title }}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		name string
		f    *print.Format
	}{{"large", rec}, {"small", small}} {
		for _, as := range []string{"text", "html", "pdf"} {
			t.Run(f.name+"/"+as, func(t *testing.T) {
				r := gin.New()
				r.Use(knet.CompressMiddleware())
				r.GET("/p/:format_name", NewPrintHandler(&fakePrint{f: f.f}))
				srv := httptest.NewServer(r)
				defer srv.Close()
				url := srv.URL + "/p/a?record=Sale&name=S-1&as=" + as

				plainResp, plain := rawGet(t, url, nil)
				if plainResp.Header.Get("Content-Encoding") != "" {
					t.Fatalf("plain request encoded: %q", plainResp.Header.Get("Content-Encoding"))
				}
				gzResp, gz := rawGet(t, url, map[string]string{"Accept-Encoding": "gzip"})
				if !bytes.Equal(decodedBody(t, gzResp, gz), plain) {
					t.Fatalf("gzip response differs from plain (%d vs %d bytes)", len(gz), len(plain))
				}
				if cl := gzResp.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(gz)) {
					t.Fatalf("Content-Length %s != wire bytes %d", cl, len(gz))
				}
				if gzResp.Header.Get("Content-Type") != plainResp.Header.Get("Content-Type") {
					t.Fatalf("Content-Type changed: %q vs %q", gzResp.Header.Get("Content-Type"), plainResp.Header.Get("Content-Type"))
				}
			})
		}
	}
}

// Stored-file downloads set Content-Length (and Content-Range) themselves; they
// must stay exact for full and ranged reads when the client accepts gzip.
func TestStoredFileUnderGzip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend, err := storage.New(storage.Config{Backend: "local", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	inner := fileVolumeAPIRouter(backend, "tenant-gz")
	content := []byte(strings.Repeat("0123456789abcdef", 300))
	key, _ := uploadVolumeFixture(t, inner, content)

	outer := gin.New()
	outer.Use(knet.CompressMiddleware())
	outer.Any("/*any", gin.WrapH(inner))
	srv := httptest.NewServer(outer)
	defer srv.Close()
	url := srv.URL + "/api/v1/files/" + key

	tests := []struct {
		name       string
		hdr        map[string]string
		wantStatus int
		want       []byte
	}{
		{"full read with gzip accepted", map[string]string{"Accept-Encoding": "gzip"}, 200, content},
		{"full read without gzip", nil, 200, content},
		{"range with gzip accepted", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=16-47"}, 206, content[16:48]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := rawGet(t, url, tt.hdr)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if cl := resp.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(body)) {
				t.Fatalf("Content-Length %s != wire bytes %d", cl, len(body))
			}
			if got := decodedBody(t, resp, body); !bytes.Equal(got, tt.want) {
				t.Fatalf("body = %d bytes, want %d", len(got), len(tt.want))
			}
			if tt.wantStatus == 206 && (resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Range") == "") {
				t.Fatalf("ranged response altered: encoding %q range %q", resp.Header.Get("Content-Encoding"), resp.Header.Get("Content-Range"))
			}
		})
	}
}
