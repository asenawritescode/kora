//go:build integration

package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/policy"
)

// TestDashboardOverrideOnDisposableDatabases serves the dashboards of an active configuration through the
// real routes, then checks the override round trip, the caller's permission filter, the administrator gate
// and the audit entries, on MySQL and PostgreSQL.
func TestDashboardOverrideOnDisposableDatabases(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database, router := openDashboardDatabase(t, tc.dialect, dsn)

			// Cashier sees Sale only, so the stock tile and the combined chart are dropped.
			assertDashboard(t, router, "Cashier", "/api/v1/dashboards/shop", "", http.StatusOK, []string{"sales"}, []string{"by_day", "sales_line"}, "last_30_days")
			do(t, router, "Cashier", http.MethodGet, "/api/v1/dashboards/stock", "", http.StatusNotFound)

			do(t, router, "Cashier", http.MethodPut, "/api/v1/dashboards/shop/override", `{}`, http.StatusForbidden)
			do(t, router, "Cashier", http.MethodDelete, "/api/v1/dashboards/shop/override", "", http.StatusForbidden)
			do(t, router, doctype.AdminRole, http.MethodPut, "/api/v1/dashboards/shop/override", `{"default_range":"forever"}`, http.StatusBadRequest)
			do(t, router, doctype.AdminRole, http.MethodPut, "/api/v1/dashboards/missing/override", `{}`, http.StatusNotFound)

			do(t, router, doctype.AdminRole, http.MethodPut, "/api/v1/dashboards/shop/override",
				`{"hidden":["by_day","ghost"],"order":["sales_line"],"default_range":"this_month"}`, http.StatusOK)
			assertDashboard(t, router, "Cashier", "/api/v1/dashboards/shop", "", http.StatusOK, []string{"sales"}, []string{"sales_line"}, "this_month")

			// A save replaces the whole override: the previous hide and order are gone.
			do(t, router, doctype.AdminRole, http.MethodPut, "/api/v1/dashboards/shop/override", `{"order":["ghost"]}`, http.StatusOK)
			assertDashboard(t, router, "Cashier", "/api/v1/dashboards/shop", "", http.StatusOK, []string{"sales"}, []string{"by_day", "sales_line"}, "last_30_days")

			// Hiding a metric name removes its tile; the dashboard stays visible while a chart remains.
			do(t, router, doctype.AdminRole, http.MethodPut, "/api/v1/dashboards/shop/override", `{"hidden":["sales"]}`, http.StatusOK)
			assertDashboard(t, router, "Cashier", "/api/v1/dashboards/shop", "", http.StatusOK, nil, []string{"by_day", "sales_line"}, "last_30_days")

			assertDashboardList(t, router, "Cashier", "shop", []string{"by_day", "sales_line"})
			do(t, router, doctype.AdminRole, http.MethodDelete, "/api/v1/dashboards/shop/override", "", http.StatusNoContent)
			assertDashboard(t, router, "Cashier", "/api/v1/dashboards/shop", "", http.StatusOK, []string{"sales"}, []string{"by_day", "sales_line"}, "last_30_days")

			var saves, resets int
			if err := database.QueryRow(kdb.Rebind(kdb.Resolve(tc.dialect), `SELECT COUNT(*) FROM _kora_operation_audit WHERE doctype = '_kora_dashboard_override' AND command_name = 'dashboard.override.save' AND actor_roles LIKE ?`), "%"+doctype.AdminRole+"%").Scan(&saves); err != nil {
				t.Fatal("count save audit:", err)
			}
			if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_operation_audit WHERE doctype = '_kora_dashboard_override' AND command_name = 'dashboard.override.reset'`).Scan(&resets); err != nil {
				t.Fatal("count reset audit:", err)
			}
			if saves != 3 || resets != 1 {
				t.Fatalf("audit rows: saves=%d resets=%d, want 3 and 1 (refused and invalid writes are not audited)", saves, resets)
			}
		})
	}
}

// openDashboardDatabase opens a disposable database with the system tables, one active configuration and the real
// metric and dashboard routes.
func openDashboardDatabase(t *testing.T, dialect, dsn string) (*sql.DB, *gin.Engine) {
	t.Helper()
	database := disposabledb.Open(t, dialect, dsn).DB
	kernel := kdb.KernelTablesMySQL()
	if dialect == "postgres" {
		kernel = kdb.KernelTablesPostgres()
	}
	// createTableStatements keeps only CREATE TABLE, so the dashboard's pinned-column ALTER runs here, as bootstrap runs it.
	var alters []string
	for _, ddl := range kdb.Resolve(dialect).SystemTableSQL() {
		if strings.HasPrefix(strings.TrimSpace(ddl), "ALTER TABLE") && strings.Contains(ddl, "_kora_dashboard_override") {
			alters = append(alters, ddl)
		}
	}
	for _, ddl := range append(append(createTableStatements(kdb.Resolve(dialect).SystemTableSQL()), alters...), kernel...) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create system tables: %v", err)
		}
	}
	seedDashboardConfig(t, database, dialect)
	return database, dashboardServer(t, database, dialect)
}

// seedDashboardConfig writes one active configuration version holding two metrics and two dashboards.
func seedDashboardConfig(t *testing.T, database *sql.DB, dialect string) {
	t.Helper()
	metricDefs := []metrics.Metric{
		{Name: "sales", Label: "Sales", Type: metrics.KindSimple, Model: "Sale", Measure: &metrics.Measure{Agg: "sum", Field: "total"}},
		{Name: "stock_qty", Label: "Stock", Type: metrics.KindSimple, Model: "Stock", Measure: &metrics.Measure{Agg: "sum", Field: "qty"}},
	}
	dashboards := []metrics.Dashboard{
		{Name: "shop", Label: "Shop", DefaultRange: "last_30_days",
			Tiles: []metrics.Tile{{Metric: "sales"}, {Metric: "stock_qty"}},
			Charts: []metrics.Chart{
				{ID: "by_day", Title: "By day", Kind: "bar", Metrics: []string{"sales"}, By: "time", Grain: "day"},
				{ID: "sales_line", Title: "Line", Kind: "line", Metrics: []string{"sales"}, By: "time", Grain: "day"},
				{ID: "combined", Title: "Both", Kind: "bar", Metrics: []string{"sales", "stock_qty"}, By: "time", Grain: "day"},
			}},
		{Name: "stock", Label: "Stock", DefaultRange: "today", Tiles: []metrics.Tile{{Metric: "stock_qty"}}},
	}
	snapshot := doctype.ConfigSnapshot{}
	for _, m := range metricDefs {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Metrics = append(snapshot.Metrics, raw)
	}
	for _, d := range dashboards {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Dashboards = append(snapshot.Dashboards, raw)
	}
	config, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	query := kdb.Rebind(kdb.Resolve(dialect), `INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, status, config) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if _, err := database.Exec(query,
		"cfg-dashboards", "tenant-dashboards", 1, time.Now().UTC(), "tester", "Active", "Active", string(config)); err != nil {
		t.Fatal("insert active config version:", err)
	}
}

// dashboardServer mounts the real metric and dashboard routes. The caller is named by the X-Roles header.
func dashboardServer(t *testing.T, database *sql.DB, dialect string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	sale := &doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "total"}}}}
	stock := &doctype.DocType{Name: "Stock", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "qty"}}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{sale, stock}, []*doctype.Role{{Name: "Cashier"}}, []*doctype.Permission{
		{Doctype: "Sale", Role: "Cashier", Read: true},
		{Doctype: "Sale", Role: doctype.AdminRole, Read: true},
		{Doctype: "Stock", Role: doctype.AdminRole, Read: true},
	})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		roles := strings.Split(c.GetHeader("X-Roles"), ",")
		execution := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-dashboards", roles, nil)
		ex, _ := execution.Get("policy_execution")
		c.Set("site_name", "tenant-dashboards")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		c.Set("policy_execution", ex.(policy.Execution))
		c.Next()
	})
	registerMetricsRoutes(router.Group("/api/v1"), registry, database, kdb.Resolve(dialect))
	return router
}

// do sends one request as the role and checks the status.
func do(t *testing.T, router *gin.Engine, role, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Roles", role)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != want {
		t.Fatalf("%s %s as %s = %d, want %d: %s", method, path, role, recorder.Code, want, recorder.Body)
	}
	return recorder
}

// assertDashboard reads one dashboard as role and checks the tiles, the chart ids and the default range.
func assertDashboard(t *testing.T, router *gin.Engine, role, path, body string, status int, tiles, charts []string, rng string) {
	t.Helper()
	recorder := do(t, router, role, http.MethodGet, path, body, status)
	var envelope struct {
		Data contract.DashboardDefinition `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode dashboard:", err)
	}
	got := envelope.Data
	if names := tileNames(got); !sameStrings(names, tiles) {
		t.Fatalf("tiles = %v, want %v", names, tiles)
	}
	if ids := chartIDs(got); !sameStrings(ids, charts) {
		t.Fatalf("charts = %v, want %v", ids, charts)
	}
	if got.DefaultRange != rng {
		t.Fatalf("default range = %q, want %q", got.DefaultRange, rng)
	}
}

// assertDashboardList checks that the list shows name with the given charts for role.
func assertDashboardList(t *testing.T, router *gin.Engine, role, name string, charts []string) {
	t.Helper()
	recorder := do(t, router, role, http.MethodGet, "/api/v1/dashboards", "", http.StatusOK)
	var envelope struct {
		Data []contract.DashboardDefinition `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode dashboard list:", err)
	}
	for _, d := range envelope.Data {
		if d.Name == name {
			if ids := chartIDs(d); !sameStrings(ids, charts) {
				t.Fatalf("listed charts = %v, want %v", ids, charts)
			}
			return
		}
	}
	t.Fatalf("list does not show %q: %s", name, recorder.Body)
}

func chartIDs(def contract.DashboardDefinition) []string {
	var out []string
	for _, ch := range def.Charts {
		out = append(out, ch.ID)
	}
	return out
}

// sameStrings compares two lists, treating nil and empty as equal.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDashboardPinsOnDisposableDatabases checks pins on MySQL and PostgreSQL: only the administrator adds or
// removes one, a caller sees only the pins whose record types it can read, the limit is four, and saving or
// resetting the layout keeps the pins.
func TestDashboardPinsOnDisposableDatabases(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database, router := openDashboardDatabase(t, tc.dialect, dsn)
			const pins = "/api/v1/dashboards/shop/pins"

			do(t, router, "Cashier", http.MethodPost, pins, pinBody("Sales", "Sale"), http.StatusForbidden)
			do(t, router, doctype.AdminRole, http.MethodPost, pins, pinBody("   ", "Sale"), http.StatusUnprocessableEntity)
			do(t, router, doctype.AdminRole, http.MethodPost, pins, pinBody("Ghost", "Ghost"), http.StatusUnprocessableEntity)
			do(t, router, doctype.AdminRole, http.MethodPost, "/api/v1/dashboards/missing/pins", pinBody("Sales", "Sale"), http.StatusNotFound)

			do(t, router, doctype.AdminRole, http.MethodPost, pins, pinBody("Sales", "Sale"), http.StatusCreated)
			stock := pinOf(t, do(t, router, doctype.AdminRole, http.MethodPost, pins, pinBody("Stock", "Stock"), http.StatusCreated))
			do(t, router, doctype.AdminRole, http.MethodPost, pins, pinBody("Sales 2", "Sale"), http.StatusCreated)
			do(t, router, doctype.AdminRole, http.MethodPost, pins, pinBody("Sales 3", "Sale"), http.StatusCreated)
			do(t, router, doctype.AdminRole, http.MethodPost, pins, pinBody("Fifth", "Sale"), http.StatusConflict)

			assertPinLabels(t, router, "Cashier", []string{"Sales", "Sales 2", "Sales 3"})
			assertPinLabels(t, router, doctype.AdminRole, []string{"Sales", "Stock", "Sales 2", "Sales 3"})

			do(t, router, doctype.AdminRole, http.MethodPut, "/api/v1/dashboards/shop/override", `{"hidden":["sales"],"default_range":"this_month"}`, http.StatusOK)
			assertPinLabels(t, router, doctype.AdminRole, []string{"Sales", "Stock", "Sales 2", "Sales 3"})
			do(t, router, doctype.AdminRole, http.MethodDelete, "/api/v1/dashboards/shop/override", "", http.StatusNoContent)
			assertPinLabels(t, router, doctype.AdminRole, []string{"Sales", "Stock", "Sales 2", "Sales 3"})

			do(t, router, "Cashier", http.MethodDelete, pins+"/"+stock.ID, "", http.StatusForbidden)
			do(t, router, doctype.AdminRole, http.MethodDelete, pins+"/"+stock.ID, "", http.StatusNoContent)
			do(t, router, doctype.AdminRole, http.MethodDelete, pins+"/"+stock.ID, "", http.StatusNotFound)
			assertPinLabels(t, router, doctype.AdminRole, []string{"Sales", "Sales 2", "Sales 3"})

			var adds, removes int
			if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_operation_audit WHERE doctype = '_kora_dashboard_override' AND command_name = 'dashboard.pin.add'`).Scan(&adds); err != nil {
				t.Fatal("count add audit:", err)
			}
			if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_operation_audit WHERE doctype = '_kora_dashboard_override' AND command_name = 'dashboard.pin.remove'`).Scan(&removes); err != nil {
				t.Fatal("count remove audit:", err)
			}
			if adds != 4 || removes != 1 {
				t.Fatalf("audit rows: adds=%d removes=%d, want 4 and 1 (refused writes are not audited)", adds, removes)
			}
		})
	}
}

// pinBody is a pin request asking for the count of one record type.
func pinBody(label, recordType string) string {
	return fmt.Sprintf(`{"label":%q,"question":{"record_type":%q}}`, label, recordType)
}

// pinOf decodes the pin a create answered with.
func pinOf(t *testing.T, recorder *httptest.ResponseRecorder) contract.DashboardPin {
	t.Helper()
	var envelope struct {
		Data contract.DashboardPin `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode pin:", err)
	}
	return envelope.Data
}

// assertPinLabels reads the shop dashboard as role and checks the labels of its pins, in order.
func assertPinLabels(t *testing.T, router *gin.Engine, role string, want []string) {
	t.Helper()
	recorder := do(t, router, role, http.MethodGet, "/api/v1/dashboards/shop", "", http.StatusOK)
	var envelope struct {
		Data contract.DashboardDefinition `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal("decode dashboard:", err)
	}
	var got []string
	for _, p := range envelope.Data.Pinned {
		got = append(got, p.Label)
	}
	if !sameStrings(got, want) {
		t.Fatalf("pins as %s = %v, want %v", role, got, want)
	}
}
