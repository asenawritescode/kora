package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// TestExploreCatalogOffersOnlyReadableFactRecordTypes checks the catalog against the grants: a declared
// model the caller may not read is left out, a generated model the caller may read is listed, and a
// child table never is.
func TestExploreCatalogOffersOnlyReadableFactRecordTypes(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{
		{Name: "Expense", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}},
		{Name: "Stock", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "qty"}}}},
		{Name: "Line", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}},
	}, []*doctype.Role{{Name: "Cashier"}}, []*doctype.Permission{
		{Doctype: "Expense", Role: "Cashier", Read: true},
		{Doctype: "Line", Role: "Cashier", Read: true},
	})

	gin.SetMode(gin.TestMode)
	execution := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-explore", []string{"Cashier"}, nil)
	ex, _ := execution.Get("policy_execution")
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", "tenant-explore")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		c.Set("policy_execution", ex.(policy.Execution))
		c.Next()
	})
	registerExploreRoutes(router.Group("/api/v1"), registry)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/explore/catalog", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	var body struct {
		Data []struct {
			RecordType string `json:"record_type"`
			Measures   []struct {
				Key string `json:"key"`
			} `json:"measures"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0].RecordType != "Expense" {
		t.Fatalf("catalog = %s, want only Expense", recorder.Body)
	}
	var keys []string
	for _, m := range body.Data[0].Measures {
		keys = append(keys, m.Key)
	}
	if strings.Join(keys, ",") != "count,sum:amount" {
		t.Fatalf("measures = %v, want count and sum:amount", keys)
	}
}
