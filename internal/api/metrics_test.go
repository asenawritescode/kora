package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// metricsFixture is the POS metric pack over a Sale record type the reader may read, a Stock record
// type it may not, and a derived metric that reads both.
func metricsFixture(t *testing.T, perms []*doctype.Permission) (*doctype.Registry, metricsAPI) {
	t.Helper()
	defs, err := metrics.ParseDirectory("../../config/pos/metrics")
	if err != nil {
		t.Fatal("parse pos metrics:", err)
	}
	defs = append(defs,
		metrics.Metric{Name: "stock_qty", Label: "Stock", Type: metrics.KindSimple, Model: "Stock", Measure: &metrics.Measure{Agg: "sum", Field: "qty"}},
		metrics.Metric{Name: "sales_and_stock", Label: "Mixed", Type: metrics.KindDerived, Formula: "sales + stock_qty"},
	)
	sale := &doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{
		Time: "creation", StatusField: "status", Measures: []doctype.FactMeasure{{Field: "total"}},
		Dimensions: []doctype.FactDimension{{Field: "register"}},
	}}
	stock := &doctype.DocType{Name: "Stock", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "qty"}}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{sale, stock}, []*doctype.Role{{Name: "Cashier"}}, perms)
	api := metricsAPI{registry: registry, defs: func(*gin.Context) ([]metrics.Metric, error) { return defs, nil }}
	return registry, api
}

// serveMetrics sends one request through the metric routes as the execution's caller.
func serveMetrics(t *testing.T, registry *doctype.Registry, api metricsAPI, roles []string, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	execution := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-metrics", roles, nil)
	ex, _ := execution.Get("policy_execution")
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", "tenant-metrics")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		c.Set("policy_execution", ex.(policy.Execution))
		c.Next()
	})
	api.mount(router.Group("/api/v1/metrics"))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	return recorder
}

func TestMetricsListAndDescribeHideUnreadableMetrics(t *testing.T) {
	registry, api := metricsFixture(t, []*doctype.Permission{{Doctype: "Sale", Role: "Cashier", Read: true}})
	list := serveMetrics(t, registry, api, []string{"Cashier"}, "GET", "/api/v1/metrics", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body)
	}
	var envelope struct {
		Data []struct{ Name string } `json:"data"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode list:", err)
	}
	var names []string
	for _, m := range envelope.Data {
		names = append(names, m.Name)
	}
	if want := []string{"average_sale", "items_sold", "receipts", "refunds", "sales"}; !slices.Equal(names, want) {
		t.Fatalf("listed %v, want %v: the stock metric and the mixed derived metric are unreadable", names, want)
	}
	if got := serveMetrics(t, registry, api, []string{"Cashier"}, "GET", "/api/v1/metrics/sales", "").Code; got != http.StatusOK {
		t.Fatalf("describe readable sales status=%d", got)
	}
	for _, name := range []string{"stock_qty", "sales_and_stock"} {
		if got := serveMetrics(t, registry, api, []string{"Cashier"}, "GET", "/api/v1/metrics/"+name, "").Code; got != http.StatusNotFound {
			t.Fatalf("describe %s status=%d, want 404", name, got)
		}
	}
}

func TestMetricsQueryChecksAuthorizationThenShape(t *testing.T) {
	registry, api := metricsFixture(t, []*doctype.Permission{{Doctype: "Sale", Role: "Cashier", Read: true}})
	query := func(body string) *httptest.ResponseRecorder {
		return serveMetrics(t, registry, api, []string{"Cashier"}, "POST", "/api/v1/metrics/query", body)
	}
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"unreadable simple metric is forbidden", `{"metrics":["stock_qty"],"from":"2026-11-18","to":"2026-11-20","grain":"day"}`, http.StatusForbidden, "metrics.forbidden"},
		{"derived metric with an unreadable component is forbidden", `{"metrics":["sales_and_stock"],"from":"2026-11-18","to":"2026-11-20","grain":"day"}`, http.StatusForbidden, "metrics.forbidden"},
		{"undefined metric is a bad query", `{"metrics":["nope"],"from":"2026-11-18","to":"2026-11-20","grain":"day"}`, http.StatusBadRequest, "metrics.query_invalid"},
		{"range over the day cap is refused", `{"metrics":["sales"],"from":"2026-01-01","to":"2027-01-02","grain":"day"}`, http.StatusBadRequest, "metrics.range_too_large"},
		{"range over the month cap is refused", `{"metrics":["sales"],"from":"2016-01-01","to":"2026-01-01","grain":"month"}`, http.StatusBadRequest, "metrics.range_too_large"},
		{"unparseable date is a bad query", `{"metrics":["sales"],"from":"18/11/2026","to":"2026-11-20","grain":"day"}`, http.StatusBadRequest, "metrics.query_invalid"},
		{"readable and valid reaches the database step", `{"metrics":["sales"],"from":"2026-11-18","to":"2026-11-20","grain":"day"}`, http.StatusServiceUnavailable, "server.store_unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := query(tc.body)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body, tc.wantStatus)
			}
			if tc.wantCode != "" && !strings.Contains(recorder.Body.String(), tc.wantCode) {
				t.Fatalf("body=%s, want code %s", recorder.Body, tc.wantCode)
			}
		})
	}
}

func TestMetricScopeNarrowsOwnerOnlyGrants(t *testing.T) {
	tests := []struct {
		name      string
		perms     []*doctype.Permission
		models    []string
		wantOwner bool
		wantErr   bool
	}{
		{"full read is unscoped", []*doctype.Permission{{Doctype: "Sale", Role: "Cashier", Read: true}}, []string{"Sale"}, false, false},
		{"owner-only read narrows to the caller", []*doctype.Permission{{Doctype: "Sale", Role: "Cashier", Read: true, IfOwner: true}}, []string{"Sale"}, true, false},
		{"one unreadable model refuses the whole query", []*doctype.Permission{{Doctype: "Sale", Role: "Cashier", Read: true}}, []string{"Sale", "Stock"}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry, _ := metricsFixture(t, tc.perms)
			ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-metrics", []string{"Cashier"}, nil)
			ex, _ := ctx.Get("policy_execution")
			ctx.Set("policy_execution", ex)
			scope, denial := metricScope(ctx, registry, tc.models)
			if (denial != nil) != tc.wantErr {
				t.Fatalf("denial = %v, wantErr %v", denial, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got := scope.Owner != ""; got != tc.wantOwner {
				t.Fatalf("owner scope = %q, want owner set %v", scope.Owner, tc.wantOwner)
			}
		})
	}
}

func TestMetricRowsEncodeNullAndExactDecimals(t *testing.T) {
	res := metrics.Result{
		Metrics: []string{"sales", "average_sale"},
		Rows: []metrics.Row{{
			Period: dayOf(2026, 11, 19),
			Values: []decimal.NullDecimal{{Decimal: decimal.RequireFromString("12.50"), Valid: true}, {}},
		}},
	}
	out, err := json.Marshal(resultOf(res))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"metrics":["sales","average_sale"],"dimensions":[],"rows":[{"bucket":"2026-11-19","group":[],"values":["12.5",null]}]}`
	if string(out) != want {
		t.Fatalf("result = %s\nwant    %s", out, want)
	}
}

// dayOf is the business date at UTC midnight, the form metric results carry.
func dayOf(y int, m time.Month, d int) tenanttime.Date {
	return dateOf(time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
}

func TestMetricDefsCacheParsesOncePerVersion(t *testing.T) {
	type step struct {
		site    string
		version int64
		wantHit bool
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"same site and version reuse the parse", []step{{"a", 1, false}, {"a", 1, true}, {"a", 1, true}}},
		{"a new version reparses", []step{{"a", 1, false}, {"a", 2, false}, {"a", 2, true}}},
		{"another site parses on its own", []step{{"a", 1, false}, {"b", 1, false}, {"a", 1, true}, {"b", 1, true}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cache := newVersionCache[[]metrics.Metric]()
			for i, s := range tc.steps {
				parses := 0
				_, err := cache.load(s.site, s.version, func() ([]metrics.Metric, error) {
					parses++
					return []metrics.Metric{{Name: "m"}}, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if hit := parses == 0; hit != s.wantHit {
					t.Fatalf("step %d (%s v%d): cache hit = %v, want %v", i, s.site, s.version, hit, s.wantHit)
				}
			}
		})
	}
}

func TestMetricDefsCacheDoesNotKeepFailedParse(t *testing.T) {
	cache := newVersionCache[[]metrics.Metric]()
	if _, err := cache.load("a", 1, func() ([]metrics.Metric, error) { return nil, errors.New("bad config") }); err == nil {
		t.Fatal("failed parse returned no error")
	}
	parsed := false
	if _, err := cache.load("a", 1, func() ([]metrics.Metric, error) { parsed = true; return nil, nil }); err != nil || !parsed {
		t.Fatalf("after a failed parse the next load must parse again: parsed=%v err=%v", parsed, err)
	}
}
