package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
