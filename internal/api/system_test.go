package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/internal/auth"
	"github.com/gin-gonic/gin"
)

func TestHandleAuthProvidersUsesRegistry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandler(nil, nil)
	h.AuthProviders.Register(auth.AuthProvider{
		Name:        "saml",
		Label:       "SAML 2.0",
		Status:      "planned",
		Description: "Enterprise federation",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/auth/providers", nil)

	h.HandleAuthProviders(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want map[string]any", resp.Data)
	}

	rawProviders, ok := data["providers"].([]any)
	if !ok {
		t.Fatalf("providers type = %T, want []any", data["providers"])
	}

	foundOIDC := false
	for _, raw := range rawProviders {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if item["name"] == "oidc" {
			foundOIDC = true
			break
		}
	}
	if !foundOIDC {
		t.Fatalf("expected oidc provider in response: %#v", rawProviders)
	}
}
