//go:build integration

package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// erpsellPack is the selling pack as the server loads it: its doctypes, metrics and dashboards.
const erpsellPack = "../../config/_parked/erpsell"

// stockMovement is one Stock Movement written as the event consumer receives it.
type stockMovement struct {
	record, product, created, change, reason, source, owner string
}

// stockSeed: a damage and a count correction (Stock Adjustment) and one sale movement, as the erpsell
// stock_adjusted metric reads them.
var stockSeed = []stockMovement{
	{"ADJ-1", "P1", "2026-11-18 12:00:00", "-2", "Damaged", "Stock Adjustment", "wh1"},
	{"ADJ-2", "P2", "2026-11-19 12:00:00", "3", "Count correction", "Stock Adjustment", "wh2"},
	{"MOV-9", "P1", "2026-11-18 13:00:00", "-1", "Sale", "Sale", "wh1"},
	{"ADJ-3", "P3", "2026-11-18 14:00:00", "1", "", "Stock Adjustment", "wh2"},
}

// stockAdmission grants every maintenance lane at once; the refresher only needs one.
type stockAdmission struct{}

func (stockAdmission) AcquireMaintenance(context.Context) (func(), error) { return func() {}, nil }

// TestStockAdjustedChartOverHTTP runs the erpsell stock_adjusted chart's query through the real metric routes over
// seeded stock movements, on MySQL and PostgreSQL. Each request must answer 200; a failure prints the log lines the
// handler wrote, which name the cause of an internal error.
func TestStockAdjustedChartOverHTTP(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			router := erpsellRouter(t, tc.dialect, dsn)
			captureLogs(t)
			for _, body := range []string{
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"range"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"day"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"period":{"preset":"last_30_days"},"grain":"range"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"period":{"preset":"last_30_days"},"grain":"day"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"period":{"preset":"last_30_days"},"grain":"day","compare":"previous_period"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"period":{"preset":"last_30_days"},"grain":"range","compare":"previous_period"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"period":{"preset":"today"},"grain":"day","compare":"week_before"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"range","compare":"previous_period"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"week"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"month"}`,
				`{"metrics":["stock_adjusted"],"dimensions":["reason"],"period":{"preset":"last_30_days"},"grain":"range","top":10}`,
			} {
				do(t, router, doctype.AdminRole, http.MethodPost, "/api/v1/metrics/query", body, http.StatusOK)
			}
			assertStockDashboards(t, router)
		})
	}
}

// assertStockDashboards checks where the stock adjustments chart lives: Stock insights shows it for today, and
// Shop insights, which is sales only, no longer carries it.
func assertStockDashboards(t *testing.T, router *gin.Engine) {
	t.Helper()
	var stock struct {
		Data contract.DashboardDefinition `json:"data"`
	}
	recorder := do(t, router, doctype.AdminRole, http.MethodGet, "/api/v1/dashboards/stock_insights", "", http.StatusOK)
	if err := json.Unmarshal(recorder.Body.Bytes(), &stock); err != nil {
		t.Fatal("decode stock_insights:", err)
	}
	if stock.Data.Pack != "Selling" {
		t.Fatalf("stock_insights pack = %q; want the selling pack's label", stock.Data.Pack)
	}
	if got := chartIDs(stock.Data); !slices.Equal(got, []string{"stock_adjustments"}) || stock.Data.DefaultRange != "today" {
		t.Fatalf("stock_insights charts = %v range %q; want stock_adjustments and today", got, stock.Data.DefaultRange)
	}
	var shop struct {
		Data contract.DashboardDefinition `json:"data"`
	}
	recorder = do(t, router, doctype.AdminRole, http.MethodGet, "/api/v1/dashboards/shop_insights", "", http.StatusOK)
	if err := json.Unmarshal(recorder.Body.Bytes(), &shop); err != nil {
		t.Fatal("decode shop_insights:", err)
	}
	if shop.Data.Pack != "Selling" {
		t.Fatalf("shop_insights pack = %q; want Selling", shop.Data.Pack)
	}
	if slices.Contains(chartIDs(shop.Data), "stock_adjustments") || shop.Data.Label != "Shop insights" {
		t.Fatalf("shop_insights = %q charts %v; want Shop insights without stock_adjustments", shop.Data.Label, chartIDs(shop.Data))
	}
}

// erpsellRouter opens a disposable database with the selling pack's configuration, consumes the seeded stock
// movements and returns the real routes.
func erpsellRouter(t *testing.T, dialect, dsn string) *gin.Engine {
	t.Helper()
	database, reg := openErpsell(t, dialect, dsn, nil, nil)
	seedStockMovements(context.Background(), t, database, dialect, reg)
	return stockRouter(t, reg, database, dialect)
}

// openErpsell opens a disposable database with the system tables and the selling pack's metrics and dashboards as the
// active configuration. The fact tables do not exist yet: the sync has not run.
func openErpsell(t *testing.T, dialect, dsn string, roles []*doctype.Role, perms []*doctype.Permission) (*sql.DB, *doctype.Registry) {
	t.Helper()
	ctx := context.Background()
	database := disposabledb.Open(t, dialect, dsn).DB
	if err := site.BootstrapSystemTables(ctx, database, kdb.Resolve(dialect)); err != nil {
		t.Fatalf("bootstrap system tables: %v", err)
	}
	dts, err := doctype.ParseConfigTree(erpsellPack)
	if err != nil {
		t.Fatalf("parse selling pack: %v", err)
	}
	defs, err := metrics.ParseDirectory(erpsellPack + "/metrics")
	if err != nil {
		t.Fatalf("parse selling metrics: %v", err)
	}
	dashboards, err := metrics.ParsePackDashboards(erpsellPack)
	if err != nil {
		t.Fatalf("parse selling dashboards: %v", err)
	}
	writeActiveConfig(t, database, dialect, dts, defs, dashboards)
	reg := doctype.NewRegistry()
	shipped, err := doctype.ParsePermissionsFile(erpsellPack + "/permissions.yaml")
	if err != nil {
		t.Fatalf("parse selling pack permissions: %v", err)
	}
	perms = append(append(shipped, perms...), &doctype.Permission{Doctype: "Stock Movement", Role: doctype.AdminRole, Read: true})
	roles = append(roles, &doctype.Role{Name: doctype.AdminRole})
	perms = append(perms,
		&doctype.Permission{Doctype: "Stock Movement", Role: doctype.AdminRole, Read: true},
		&doctype.Permission{Doctype: "Sale", Role: doctype.AdminRole, Read: true})
	reg.LoadFull(dts, roles, perms)
	return database, reg
}

// writeActiveConfig stores the metrics and dashboards as one active configuration version of tenant-dashboards.
// EncodePack checks them against the selling pack's record types first, as the import does.
func writeActiveConfig(t *testing.T, database *sql.DB, dialect string, dts []*doctype.DocType, defs []metrics.Metric, dashboards []metrics.Dashboard) {
	t.Helper()
	metricDocs, dashboardDocs, err := metrics.EncodePack(defs, dashboards, dts)
	if err != nil {
		t.Fatalf("selling pack does not validate: %v", err)
	}
	config, err := json.Marshal(doctype.ConfigSnapshot{Metrics: metricDocs, Dashboards: dashboardDocs})
	if err != nil {
		t.Fatal(err)
	}
	query := kdb.Rebind(kdb.Resolve(dialect), `INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, status, config) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if _, err := database.Exec(query, "cfg-erpsell", "tenant-dashboards", 1, time.Now().UTC(), "tester", "Active", "Active", string(config)); err != nil {
		t.Fatal("insert active config version:", err)
	}
}

// seedStockMovements creates the Stock Movement table and tenant settings as the metric route reads them, syncs
// the fact tables the way tenant start does, and delivers each movement to the fact consumer.
func seedStockMovements(ctx context.Context, t *testing.T, database *sql.DB, dialect string, reg *doctype.Registry) {
	t.Helper()
	d := kdb.Resolve(dialect)
	setup := []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, product VARCHAR(140) NULL)", d.QuoteIdent("tabStock Movement")),
		"CREATE TABLE IF NOT EXISTS _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
	}
	for _, stmt := range setup {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	if _, err := database.ExecContext(ctx, kdb.Rebind(d, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)"), "tenant-dashboards", tenanttime.KeyTimeZone, "Africa/Nairobi"); err != nil {
		t.Fatalf("site setting: %v", err)
	}
	if err := (&facts.ModelSync{DB: database, Dialect: d, Registry: reg, Owner: "stock-http-test"}).Sync(ctx); err != nil {
		t.Fatalf("sync fact models: %v", err)
	}
	builder := &facts.Builder{DB: database, Dialect: d, Registry: reg}
	for i, m := range stockSeed {
		insert := kdb.Rebind(d, fmt.Sprintf("INSERT INTO %s (name, owner, creation, product) VALUES (?, ?, ?, ?)", d.QuoteIdent("tabStock Movement")))
		if _, err := database.ExecContext(ctx, insert, m.record, m.owner, m.created, m.product); err != nil {
			t.Fatalf("insert %s: %v", m.record, err)
		}
		consumeStockMovement(ctx, t, database, builder, int64(i+1), m)
	}
	refresher := &facts.Refresher{DB: database, Dialect: d, Registry: reg, Admission: stockAdmission{}, Owner: "stock-http-test"}
	if _, err := refresher.RefreshDue(ctx); err != nil {
		t.Fatalf("refresh summaries: %v", err)
	}
}

// consumeStockMovement delivers one movement's insert to the fact consumer in its own transaction.
func consumeStockMovement(ctx context.Context, t *testing.T, database *sql.DB, b *facts.Builder, seq int64, m stockMovement) {
	t.Helper()
	payload := map[string]any{
		"table": "Stock Movement", "record_id": m.record,
		"data": map[string]any{"product": m.product, "quantity_change": numberOf(m.change), "source_doctype": m.source},
	}
	if m.reason != "" {
		payload["data"].(map[string]any)["reason"] = m.reason
	}
	event := contract.EventEnvelope{
		ID: fmt.Sprintf("evt-%d", seq), Type: outbox.EventTypeName("Stock Movement", eventstream.EventInsert),
		Sequence: contract.Revision(strconv.FormatInt(seq, 10)), Site: "tenant-dashboards",
		OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		Data:       contract.MustEncodeData(payload),
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Consume(ctx, tx, event); err != nil {
		_ = tx.Rollback()
		t.Fatalf("consume %s: %v", m.record, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// stockRouter mounts the real metric routes for a caller named by X-Roles, on the given registry.
func stockRouter(t *testing.T, reg *doctype.Registry, database *sql.DB, dialect string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		roles := strings.Split(c.GetHeader("X-Roles"), ",")
		execution := newVerifiedTestContext(t, "stable-tenant", "tenant-dashboards", roles, nil)
		ex, _ := execution.Get("policy_execution")
		c.Set("site_name", "tenant-dashboards")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", reg)
		c.Set("policy_execution", ex.(policy.Execution))
		c.Next()
	})
	registerMetricsRoutes(router.Group("/api/v1"), reg, database, kdb.Resolve(dialect))
	return router
}

// captureLogs sends the default logger to a buffer for the rest of the test. When the test fails the buffer is
// printed, so the cause the handler logged for an internal error is in the failure output.
func captureLogs(t *testing.T) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		if t.Failed() {
			t.Logf("handler log:\n%s", buf.String())
		}
	})
}

// numberOf is the JSON number the event carries for a Float field.
func numberOf(text string) float64 {
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		panic(err)
	}
	return v
}

// TestMetricsWhileFactTablesAreBuilding queries and drills a selling pack whose sync has not run: the answer is
// empty with history building, never an error. Once the sync has run and the movements are consumed, the same
// query answers with rows.
func TestMetricsWhileFactTablesAreBuilding(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_DASHBOARD_OVERRIDE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			captureLogs(t)
			database, reg := openErpsell(t, tc.dialect, dsn, nil, nil)
			router := stockRouter(t, reg, database, tc.dialect)
			query := `{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"range"}`
			drill := `{"metric":"stock_adjusted","from":"2026-11-18","to":"2026-11-20","group":{"reason":"Damaged"}}`
			assertHistory(t, router, "/api/v1/metrics/query", query, "building")
			assertHistory(t, router, "/api/v1/metrics/drill", drill, "building")

			seedStockMovements(context.Background(), t, database, tc.dialect, reg)
			recorder := do(t, router, doctype.AdminRole, http.MethodPost, "/api/v1/metrics/query", query, http.StatusOK)
			var env metricsEnvelopeBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
				t.Fatal("decode query:", err)
			}
			// The sync queues one rebuild of the new model; the query answers from the consumed rows meanwhile.
			if !strings.Contains(string(env.Data), "Damaged") {
				t.Fatalf("after sync: data %s; want the consumed rows", env.Data)
			}
		})
	}
}

// metricsEnvelopeBody is a metrics response as a test reads it.
type metricsEnvelopeBody struct {
	Data json.RawMessage      `json:"data"`
	Meta contract.MetricsMeta `json:"meta"`
}

// assertHistory posts body to path as the administrator and checks the answer is empty with the history state.
func assertHistory(t *testing.T, router *gin.Engine, path, body, state string) {
	t.Helper()
	recorder := do(t, router, doctype.AdminRole, http.MethodPost, path, body, http.StatusOK)
	var env metricsEnvelopeBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s: decode: %v", path, err)
	}
	if env.Meta.History == nil || env.Meta.History.State != state || env.Meta.History.Progress != 0 {
		t.Fatalf("%s: history %+v; want %s at 0", path, env.Meta.History, state)
	}
	if !strings.Contains(string(env.Data), `"rows":[]`) {
		t.Fatalf("%s: data while building = %s; want no rows", path, env.Data)
	}
}
