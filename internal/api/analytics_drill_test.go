package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// The drill route is permission-gated like the query routes and answers the
// list filter for a number without reading records.
func TestAnalyticsDrillRouteGatesSourceReadAndReturnsListFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Invoice"}}, []*doctype.Role{{Name: "Reader"}, {Name: "Nobody"}}, []*doctype.Permission{
		{Doctype: "Invoice", Role: "Reader", Read: true},
	})
	var active policy.Execution
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", "tenant-drill")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		if active.Valid() {
			c.Set("policy_execution", active)
		}
		c.Next()
	})
	RegisterAnalyticsRoutes(router.Group("/api"), registry, nil, nil, AnalyticsOptions{})
	body := `{"model":"invoice","time":{"from":"2026-01-01","to":"2026-01-31"}}`
	post := func(execution policy.Execution) *httptest.ResponseRecorder {
		active = execution
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest("POST", "/api/analytics/drill", strings.NewReader(body)))
		return recorder
	}
	if got := post(policy.Execution{}).Code; got != 403 {
		t.Fatalf("missing execution status=%d, want 403", got)
	}
	nobody := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-drill", []string{"Nobody"}, nil)
	nobodyExecution, _ := nobody.Get("policy_execution")
	if got := post(nobodyExecution.(policy.Execution)).Code; got != 403 {
		t.Fatalf("unreadable source status=%d, want 403", got)
	}
	reader := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-drill", []string{"Reader"}, nil)
	readerExecution, _ := reader.Get("policy_execution")
	recorder := post(readerExecution.(policy.Execution))
	if recorder.Code != 200 {
		t.Fatalf("reader status=%d body=%s", recorder.Code, recorder.Body)
	}
	var envelope struct {
		Data struct {
			Doctype string  `json:"doctype"`
			Filters [][]any `json:"filters"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode drill response:", err)
	}
	if envelope.Data.Doctype != "Invoice" || len(envelope.Data.Filters) != 2 || envelope.Data.Filters[0][2] != "2026-01-01" || envelope.Data.Filters[1][2] != "2026-02-01" {
		t.Fatalf("drill = %+v, want Invoice with the January window", envelope.Data)
	}
}
