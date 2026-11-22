//go:build integration

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// keeperRole reads Stock Movement only for the records it owns: the owner-only grant the selling pack's Cashier
// role would need for its stock reads to be limited to its own records.
const keeperRole = "Keeper"

// TestOwnerOnlyStockReadsSeeOnlyOwnRecords seeds movements owned by two users and reads them as an owner-only role
// through the query, the drill and the dashboards. The role must see its own movements and no one else's, on
// MySQL and PostgreSQL. The administrator still sees all of them.
func TestOwnerOnlyStockReadsSeeOnlyOwnRecords(t *testing.T) {
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
			database, reg := openErpsell(t, tc.dialect, dsn, []*doctype.Role{{Name: keeperRole}}, []*doctype.Permission{
				{Doctype: "Stock Movement", Role: keeperRole, Read: true, IfOwner: true},
			})
			seedStockMovements(context.Background(), t, database, tc.dialect, reg)
			router := ownerRouter(t, reg, database, tc.dialect)

			all := rowTotals(t, doAs(t, router, "admin", doctype.AdminRole, "/api/v1/metrics/query", stockByReason))
			if all["Damaged"] != "-2" || all["Count correction"] != "3" {
				t.Fatalf("administrator totals %v; want all owners", all)
			}
			wh1 := rowTotals(t, doAs(t, router, "wh1", keeperRole, "/api/v1/metrics/query", stockByReason))
			if wh1["Damaged"] != "-2" || wh1["Count correction"] != "" {
				t.Fatalf("owner wh1 totals %v; want its own Damaged -2 and no Count correction", wh1)
			}
			wh2 := rowTotals(t, doAs(t, router, "wh2", keeperRole, "/api/v1/metrics/query", stockByReason))
			if wh2["Damaged"] != "" || wh2["Count correction"] != "3" {
				t.Fatalf("owner wh2 totals %v; want its own Count correction 3 and no Damaged", wh2)
			}
			drillDamaged := `{"metric":"stock_adjusted","from":"2026-11-18","to":"2026-11-20","group":{"reason":"Damaged"}}`
			if got := drillRecords(t, doAs(t, router, "wh2", keeperRole, "/api/v1/metrics/drill", drillDamaged)); len(got) != 0 {
				t.Fatalf("owner wh2 drill of another owner's records = %v; want none", got)
			}
			if got := drillRecords(t, doAs(t, router, "wh1", keeperRole, "/api/v1/metrics/drill", drillDamaged)); len(got) != 1 || got[0] != "ADJ-1" {
				t.Fatalf("owner wh1 drill = %v; want ADJ-1", got)
			}
			doAs(t, router, "wh1", keeperRole, "/api/v1/dashboards/stock_insights", "")
			doAs(t, router, "wh1", keeperRole, "/api/v1/metrics/query", `{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"day"}`)
		})
	}
}

// stockByReason is the stock_adjustments chart's query over the window the seeded movements fall in.
const stockByReason = `{"metrics":["stock_adjusted"],"dimensions":["reason"],"from":"2026-11-18","to":"2026-11-20","grain":"range"}`

// ownerRouter mounts the real metric routes for a caller named by the X-User and X-Roles headers. Unlike
// stockRouter, the caller's user id is the subject the owner scope reads.
func ownerRouter(t *testing.T, reg *doctype.Registry, database *sql.DB, dialect string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		user := c.GetHeader("X-User")
		actor := contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, SubjectUserID: user,
			Site: "tenant-dashboards", Roles: strings.Split(c.GetHeader("X-Roles"), ","), AuthenticatedAt: time.Now().UTC()}
		execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID("stable-tenant"), actor, nil)
		if err != nil {
			t.Errorf("verified execution: %v", err)
		}
		c.Set("site_name", "tenant-dashboards")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", reg)
		c.Set("policy_execution", execution)
		c.Next()
	})
	registerMetricsRoutes(router.Group("/api/v1"), reg, database, kdb.Resolve(dialect))
	return router
}

// doAs sends one request as user with role and checks it answers 200. A body makes it a POST; none, a GET.
func doAs(t *testing.T, router *gin.Engine, user, role, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-User", user)
	req.Header.Set("X-Roles", role)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s %s as %s = %d: %s", method, path, user, recorder.Code, recorder.Body)
	}
	return recorder
}

// rowTotals reads a one-bucket query answer as reason to value, the first value of each row. A group with no rows
// in the window is absent.
func rowTotals(t *testing.T, recorder *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var env struct {
		Data struct {
			Rows []struct {
				Group  []string `json:"group"`
				Values []string `json:"values"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
		t.Fatal("decode query:", err)
	}
	out := map[string]string{}
	for _, r := range env.Data.Rows {
		if len(r.Group) > 0 && len(r.Values) > 0 {
			out[r.Group[0]] = r.Values[0]
		}
	}
	return out
}

// drillRecords reads the record names of a drill answer.
func drillRecords(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	var env struct {
		Data struct {
			Rows []struct {
				Name string `json:"name"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
		t.Fatal("decode drill:", err)
	}
	var names []string
	for _, r := range env.Data.Rows {
		names = append(names, r.Name)
	}
	return names
}

// TestStockManagerReadsStockInsights checks the shipped selling pack lets the Stock Manager role read Stock Movement:
// the Stock insights dashboard lists its chart for that role and the chart's query answers.
func TestStockManagerReadsStockInsights(t *testing.T) {
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
			database, reg := openErpsell(t, tc.dialect, dsn, []*doctype.Role{{Name: "Stock Manager"}}, nil)
			seedStockMovements(context.Background(), t, database, tc.dialect, reg)
			router := ownerRouter(t, reg, database, tc.dialect)
			var env struct {
				Data contract.DashboardDefinition `json:"data"`
			}
			recorder := doAs(t, router, "stock-1", "Stock Manager", "/api/v1/dashboards/stock_insights", "")
			if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
				t.Fatal("decode stock_insights:", err)
			}
			if ids := chartIDs(env.Data); len(ids) != 1 || ids[0] != "stock_adjustments" {
				t.Fatalf("Stock Manager sees charts %v in stock_insights; want stock_adjustments", ids)
			}
			if got := rowTotals(t, doAs(t, router, "stock-1", "Stock Manager", "/api/v1/metrics/query", stockByReason)); got["Damaged"] != "-2" {
				t.Fatalf("Stock Manager totals %v; want Damaged -2", got)
			}
		})
	}
}
