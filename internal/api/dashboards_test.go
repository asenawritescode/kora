package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/policy"
)

// dashboardFixture has a Sale record type the Cashier may read, a Stock record type it may not, one
// metric over each, and a derived metric that reads both.
func dashboardFixture(t *testing.T) (*doctype.Registry, metricsAPI) {
	t.Helper()
	defs := []metrics.Metric{
		{Name: "sales", Label: "Sales", Type: metrics.KindSimple, Model: "Sale", Measure: &metrics.Measure{Agg: "sum", Field: "total"}},
		{Name: "stock_qty", Label: "Stock", Type: metrics.KindSimple, Model: "Stock", Measure: &metrics.Measure{Agg: "sum", Field: "qty"}},
		{Name: "sales_and_stock", Label: "Mixed", Type: metrics.KindDerived, Formula: "sales + stock_qty"},
	}
	sale := &doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "total"}}}}
	stock := &doctype.DocType{Name: "Stock", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "qty"}}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{sale, stock}, []*doctype.Role{{Name: "Cashier"}}, []*doctype.Permission{{Doctype: "Sale", Role: "Cashier", Read: true}})
	return registry, metricsAPI{registry: registry, defs: func(*gin.Context) ([]metrics.Metric, error) { return defs, nil }}
}

// serveDashboards sends one request through the dashboard routes as a caller with roles.
func serveDashboards(t *testing.T, registry *doctype.Registry, api dashboardsAPI, roles []string, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	execution := newVerifiedTestContext(t, "stable-tenant", "tenant-dashboards", roles, nil)
	ex, _ := execution.Get("policy_execution")
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", "tenant-dashboards")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		c.Set("policy_execution", ex.(policy.Execution))
		c.Next()
	})
	api.mount(router.Group("/api/v1/dashboards"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	return recorder
}

// viewAs returns dash as roles see it, before any override.
func viewAs(t *testing.T, registry *doctype.Registry, api dashboardsAPI, roles []string, dash metrics.Dashboard) (contract.DashboardDefinition, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	execution := newVerifiedTestContext(t, "stable-tenant", "tenant-dashboards", roles, nil)
	ex, _ := execution.Get("policy_execution")
	router := gin.New()
	var def contract.DashboardDefinition
	var visible bool
	router.GET("/", func(c *gin.Context) {
		c.Set("site_name", "tenant-dashboards")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		c.Set("policy_execution", ex.(policy.Execution))
		cat, err := api.catalog(c)
		if err != nil {
			t.Error(err)
			return
		}
		def, visible = api.view(c, cat, dash, nil)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	return def, visible
}

func TestDashboardViewDropsWhatTheCallerCannotRead(t *testing.T) {
	registry, base := dashboardFixture(t)
	api := dashboardsAPI{metricsAPI: base}
	dash := metrics.Dashboard{
		Name: "shop", Label: "Shop", DefaultRange: "last_30_days",
		Tiles: []metrics.Tile{{Metric: "sales"}, {Metric: "stock_qty"}},
		Charts: []metrics.Chart{
			{ID: "both", Title: "Both", Kind: "bar", Metrics: []string{"sales", "stock_qty"}, By: "time", Grain: "day"},
			{ID: "sales_only", Title: "Sales", Kind: "line", Metrics: []string{"sales"}, By: "time", Grain: "day"},
		},
	}
	def, visible := viewAs(t, registry, api, []string{"Cashier"}, dash)
	if !visible {
		t.Fatal("dashboard with a readable tile is not visible")
	}
	if got := tileNames(def); !slices.Equal(got, []string{"sales"}) {
		t.Fatalf("tiles = %v, want only the readable sales tile", got)
	}
	if len(def.Charts) != 1 || def.Charts[0].ID != "sales_only" {
		t.Fatalf("charts = %+v, want only sales_only (a chart needs every metric readable)", def.Charts)
	}

	onlyStock := metrics.Dashboard{Name: "stock", Label: "Stock", DefaultRange: "today", Tiles: []metrics.Tile{{Metric: "stock_qty"}}}
	if _, visible := viewAs(t, registry, api, []string{"Cashier"}, onlyStock); visible {
		t.Fatal("dashboard with nothing readable is visible")
	}
}

func tileNames(def contract.DashboardDefinition) []string {
	var out []string
	for _, t := range def.Tiles {
		out = append(out, t.Metric)
	}
	return out
}

func TestApplyDashboardOverride(t *testing.T) {
	base := contract.DashboardDefinition{
		Name: "shop", DefaultRange: "last_30_days",
		Tiles:  []contract.DashboardTile{{Metric: "sales"}, {Metric: "profit"}, {Metric: "receipts"}},
		Charts: []contract.DashboardChart{{ID: "by_day"}, {ID: "best"}},
	}
	tests := []struct {
		name      string
		override  contract.DashboardOverride
		tiles     []string
		charts    []string
		rangeWant string
	}{
		{"empty override keeps the pack layout", contract.DashboardOverride{}, []string{"sales", "profit", "receipts"}, []string{"by_day", "best"}, "last_30_days"},
		{"hidden tile and chart are removed", contract.DashboardOverride{Hidden: []string{"profit", "best"}}, []string{"sales", "receipts"}, []string{"by_day"}, "last_30_days"},
		{"order moves named items first and keeps the rest in pack order", contract.DashboardOverride{Order: []string{"receipts", "by_day"}}, []string{"receipts", "sales", "profit"}, []string{"by_day", "best"}, "last_30_days"},
		{"names the dashboard lacks are ignored", contract.DashboardOverride{Hidden: []string{"ghost"}, Order: []string{"ghost"}}, []string{"sales", "profit", "receipts"}, []string{"by_day", "best"}, "last_30_days"},
		{"default range replaces the pack default", contract.DashboardOverride{DefaultRange: "this_month"}, []string{"sales", "profit", "receipts"}, []string{"by_day", "best"}, "this_month"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyDashboardOverride(base, tc.override)
			if tiles := tileNames(got); !slices.Equal(tiles, tc.tiles) {
				t.Fatalf("tiles = %v, want %v", tiles, tc.tiles)
			}
			var charts []string
			for _, ch := range got.Charts {
				charts = append(charts, ch.ID)
			}
			if !slices.Equal(charts, tc.charts) {
				t.Fatalf("charts = %v, want %v", charts, tc.charts)
			}
			if got.DefaultRange != tc.rangeWant {
				t.Fatalf("default range = %q, want %q", got.DefaultRange, tc.rangeWant)
			}
		})
	}
}

func TestValidateDashboardOverride(t *testing.T) {
	tests := []struct {
		name    string
		in      contract.DashboardOverride
		wantErr bool
	}{
		{"empty is valid", contract.DashboardOverride{}, false},
		{"known range and names", contract.DashboardOverride{Hidden: []string{"sales"}, Order: []string{"by_day"}, DefaultRange: "last_7_days"}, false},
		{"unknown range", contract.DashboardOverride{DefaultRange: "forever"}, true},
		{"blank name", contract.DashboardOverride{Hidden: []string{"  "}}, true},
		{"name too long", contract.DashboardOverride{Order: []string{strings.Repeat("x", maxDashboardKeyLen+1)}}, true},
		{"name listed twice", contract.DashboardOverride{Hidden: []string{"sales", "sales"}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateDashboardOverride(tc.in); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestDashboardWritesNeedAdministrator(t *testing.T) {
	registry, base := dashboardFixture(t)
	api := dashboardsAPI{metricsAPI: base}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		body := `{"hidden":["sales"]}`
		recorder := serveDashboards(t, registry, api, []string{"Cashier"}, method, "/api/v1/dashboards/shop/override", body)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s as Cashier = %d, want 403: %s", method, recorder.Code, recorder.Body)
		}
	}
}

func TestDashboardOverrideRejectsInvalidBodyBeforeStorage(t *testing.T) {
	registry, base := dashboardFixture(t)
	api := dashboardsAPI{metricsAPI: base}
	recorder := serveDashboards(t, registry, api, []string{doctype.AdminRole}, http.MethodPut, "/api/v1/dashboards/shop/override", `{"default_range":"forever"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "dashboards.override_invalid") {
		t.Fatalf("invalid override = %d: %s", recorder.Code, recorder.Body)
	}
	recorder = serveDashboards(t, registry, api, []string{doctype.AdminRole}, http.MethodPut, "/api/v1/dashboards/shop/override", `{"hidden":`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed override = %d: %s", recorder.Code, recorder.Body)
	}
}

func TestDashboardsNeedAStoreToRead(t *testing.T) {
	registry, base := dashboardFixture(t)
	api := dashboardsAPI{metricsAPI: base, loadDefs: func(*gin.Context) ([]metrics.Dashboard, error) {
		return []metrics.Dashboard{{Name: "shop", Label: "Shop", DefaultRange: "today", Tiles: []metrics.Tile{{Metric: "sales"}}}}, nil
	}}
	recorder := serveDashboards(t, registry, api, []string{"Cashier"}, http.MethodGet, "/api/v1/dashboards", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("list without a site store = %d, want 503: %s", recorder.Code, recorder.Body)
	}
}
