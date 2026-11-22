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
	execution := newVerifiedTestContext(t, "stable-tenant", "tenant-explore", []string{"Cashier"}, nil)
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

// linkedRegistry is an Expense whose supplier is a Link to Supplier, with the grants the caller holds.
func linkedRegistry(perms []*doctype.Permission) *doctype.Registry {
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{
		{Name: "Expense", Fields: []doctype.Field{
			{Fieldname: "amount", Fieldtype: "Currency"},
			{Fieldname: "supplier", Label: "Supplier", Fieldtype: "Link", Options: "Supplier"},
		}, Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "amount"}}, Dimensions: []doctype.FactDimension{{Field: "supplier"}}}},
		{Name: "Supplier", Fields: []doctype.Field{{Fieldname: "town", Label: "Town", Fieldtype: "Data"}}},
	}, []*doctype.Role{{Name: "Cashier"}}, perms)
	return registry
}

// TestExploreCatalogLinkedDimensionsNeedReadOfTheLinkedType checks that a Link dimension opens its linked
// record's fields only when the caller may read that record type.
func TestExploreCatalogLinkedDimensionsNeedReadOfTheLinkedType(t *testing.T) {
	expenseRead := &doctype.Permission{Doctype: "Expense", Role: "Cashier", Read: true}
	supplierRead := &doctype.Permission{Doctype: "Supplier", Role: "Cashier", Read: true}
	supplierOwnerOnly := &doctype.Permission{Doctype: "Supplier", Role: "Cashier", Read: true, IfOwner: true}
	tests := []struct {
		name        string
		perms       []*doctype.Permission
		legalEntity bool
		wantPath    bool
	}{
		{"linked type readable", []*doctype.Permission{expenseRead, supplierRead}, false, true},
		{"linked type not readable", []*doctype.Permission{expenseRead}, false, false},
		{"entity-scoped caller reads tenant-shared linked type", []*doctype.Permission{expenseRead, supplierRead}, true, true},
		{"owner-only grant on linked type is refused", []*doctype.Permission{expenseRead, supplierOwnerOnly}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := linkedRegistry(tc.perms)
			gin.SetMode(gin.TestMode)
			execution := newVerifiedTestContext(t, "stable-tenant", "tenant-explore", []string{"Cashier"}, nil)
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
			req := httptest.NewRequest(http.MethodGet, "/api/v1/explore/catalog", nil)
			if tc.legalEntity {
				req.Header.Set("X-Kora-Legal-Entity-ID", "branch-nairobi")
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			if got := strings.Contains(recorder.Body.String(), `"path":"supplier.town"`); got != tc.wantPath {
				t.Fatalf("supplier.town offered = %v, want %v: %s", got, tc.wantPath, recorder.Body)
			}
		})
	}
}
