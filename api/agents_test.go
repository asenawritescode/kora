package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/org"
	"github.com/gin-gonic/gin"
)

func TestAgentManifestAndRunEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(nil, nil)
	r.PUT("/api/v1/system/agents/:id", h.HandleAgentManifest)
	r.GET("/api/v1/system/agents/:id", h.HandleAgentManifest)
	r.POST("/api/v1/system/agents/:id/runs", h.HandleAgentRuns)
	r.GET("/api/v1/system/agents/:id/runs", h.HandleAgentRuns)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/system/agents/ops", bytes.NewBufferString(`{"name":"Operations","granted_capabilities":["records.read"]}`))
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("manifest save status = %d", response.Code)
	}
	run := org.AgentRun{ID: "run-1", Capability: "records.read", Status: "completed"}
	body, _ := json.Marshal(run)
	response = httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/system/agents/ops/runs", bytes.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("run status = %d", response.Code)
	}
}

func TestAgentPermissionCatalogAndEffectivePermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "PurchaseRequest"})
	reg.LoadFull(reg.All(), []*doctype.Role{{Name: "Purchasing Operator"}}, []*doctype.Permission{{Doctype: "PurchaseRequest", Role: "Purchasing Operator", Read: true, Create: true}})
	h := NewHandler(reg, nil)
	r := gin.New()
	r.PUT("/agents/:id", h.HandleAgentManifest)
	r.GET("/agents/:id/effective-permissions", h.HandleAgentEffectivePermissions)
	r.GET("/catalog", h.HandleAgentPermissionCatalog)

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/agents/ops", bytes.NewBufferString(`{"name":"Operations","roles":["Purchasing Operator"],"direct_permissions":[{"doctype":"PurchaseRequest","operation":"write"}]}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("permission policy save status = %d, body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/agents/ops/effective-permissions", nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"source":"role:Purchasing Operator"`)) || !bytes.Contains(response.Body.Bytes(), []byte(`"source":"direct"`)) {
		t.Fatalf("effective permissions response = %d, body=%s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/catalog", nil))
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`PurchaseRequest · submit`)) {
		t.Fatalf("permission catalog response = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestAgentPermissionPolicyRejectsUnknownResources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Task"})
	h := NewHandler(reg, nil)
	r := gin.New()
	r.PUT("/agents/:id", h.HandleAgentManifest)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/agents/ops", bytes.NewBufferString(`{"name":"Operations","direct_permissions":[{"doctype":"Missing","operation":"read"}]}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown permission status = %d, body=%s", response.Code, response.Body.String())
	}
}
