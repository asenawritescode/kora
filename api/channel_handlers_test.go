package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

func TestHandleChannelToolsIncludesLoadedConfigRevisionInVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil)
	r := gin.New()
	r.GET("/tools", func(c *gin.Context) {
		c.Set("auth_type", "extension")
		c.Set("extension_name", "kora-cloud-channel")
		c.Set("site_registry", doctype.NewRegistry())
		c.Set("site_config_revision", uint64(42))
		h.HandleChannelTools(c)
	})
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("GET", "/tools?channel=web", nil))
	if response.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Version        string `json:"version"`
			ConfigRevision uint64 `json:"config_revision"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.ConfigRevision != 42 || !strings.HasPrefix(payload.Data.Version, "r42:") {
		t.Fatalf("expected revisioned catalog response, got %#v", payload.Data)
	}
	if response.Header().Get("ETag") != payload.Data.Version {
		t.Fatalf("ETag %q does not match catalog version %q", response.Header().Get("ETag"), payload.Data.Version)
	}
}
