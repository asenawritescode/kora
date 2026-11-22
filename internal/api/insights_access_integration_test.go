//go:build integration

package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/envcompat"
)

// insightsRouteCase is one Insights route and the body a request to it carries.
type insightsRouteCase struct {
	method, path, body string
}

// insightsRoutes are the metrics, explore, dashboard, drill, saved-report and pin routes Insights gates.
var insightsRoutes = []insightsRouteCase{
	{http.MethodGet, "/api/v1/metrics", ""},
	{http.MethodGet, "/api/v1/metrics/sales", ""},
	{http.MethodPost, "/api/v1/metrics/query", `{"metrics":["sales"],"period":{"preset":"today"},"grain":"range"}`},
	{http.MethodPost, "/api/v1/metrics/drill", `{"metric":"sales","from":"2026-01-01","to":"2026-01-01"}`},
	{http.MethodGet, "/api/v1/explore/catalog", ""},
	{http.MethodGet, "/api/v1/explore/reports", ""},
	{http.MethodPost, "/api/v1/explore/reports", `{"name":"x","visibility":"me","question":{"metric":"sales","grain":"range"}}`},
	{http.MethodGet, "/api/v1/dashboards", ""},
	{http.MethodPost, "/api/v1/dashboards/sales/pins", `{}`},
}

// TestInsightsRoleGate runs every Insights route as a role without Insights, as a mixed role set and as the admin,
// on the real dialect. A role without Insights gets 403 metrics.insights_forbidden; a role that grants it, or
// the admin, does not.
func TestInsightsRoleGate(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_ASK_ZANA_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_ASK_ZANA_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			captureLogs(t)
			database, reg := openErpsell(t, tc.dialect, dsn,
				[]*doctype.Role{{Name: tillCashierRole, InsightsAccess: boolPtr(false)}, {Name: "Store Manager"}},
				[]*doctype.Permission{
					{Doctype: "Sale", Role: tillCashierRole, Read: true, IfOwner: true},
					{Doctype: "Sale", Role: "Store Manager", Read: true},
				})
			router := askZanaRouter(t, database, reg, tc.dialect)
			tests := []struct {
				name, roles string
				wantStatus  int
			}{
				{"cashier is refused", tillCashierRole, http.StatusForbidden},
				{"manager keeps insights", "Store Manager", http.StatusOK},
				{"cashier with manager keeps insights", tillCashierRole + ",Store Manager", http.StatusOK},
				{"admin keeps insights", doctype.AdminRole, http.StatusOK},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					for _, r := range insightsRoutes {
						status, body := insightsCall(router, r, "shop-user", tt.roles)
						if tt.wantStatus == http.StatusForbidden {
							if status != http.StatusForbidden || !strings.Contains(body, "metrics.insights_forbidden") {
								t.Fatalf("%s %s = %d %s; want 403 metrics.insights_forbidden", r.method, r.path, status, body)
							}
							continue
						}
						// Pins are admin-only on top of Insights, so a manager may get permission.admin_required there.
						if strings.Contains(body, "metrics.insights_forbidden") {
							t.Fatalf("%s %s = %d %s; the role grants Insights", r.method, r.path, status, body)
						}
					}
				})
			}
		})
	}
}

// TestInsightsGateInChat checks the chat, for a person without Insights: the model is offered no metrics tool
// and is told Insights is not available.
func TestInsightsGateInChat(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_ASK_ZANA_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_ASK_ZANA_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			captureLogs(t)
			database, reg := openErpsell(t, tc.dialect, dsn,
				[]*doctype.Role{{Name: tillCashierRole, InsightsAccess: boolPtr(false)}},
				[]*doctype.Permission{{Doctype: "Sale", Role: tillCashierRole, Read: true, IfOwner: true}})
			model := startFakeModel(t)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", model.server.URL)
			t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "")
			t.Setenv(envcompat.IntegrationTokenEnv, "golden-token")
			router := askZanaRouter(t, database, reg, tc.dialect)

			model.script([]modelStep{{content: "Insights is not available for your role, so I can't give that number."}})
			got := askZana(t, router, "alice", tillCashierRole, "insights-gate", "How many sales did we make last week?")
			if strings.ContainsAny(got.Reply, "0123456789") || len(got.Cards) != 0 {
				t.Fatalf("reply %q cards %d; want no number and no card", got.Reply, len(got.Cards))
			}
			body := model.since(0)[0]
			for _, name := range []string{"list_metrics", "query_metrics", "drill_metrics"} {
				if strings.Contains(body, `"name":"`+name+`"`) {
					t.Fatalf("model was offered %s to a person without Insights", name)
				}
			}
			if !strings.Contains(body, "Insights is not available for this person's role") {
				t.Fatal("model was not told Insights is unavailable")
			}
		})
	}
}

// insightsCall sends one request as user with roles and returns its status and body.
func insightsCall(router http.Handler, r insightsRouteCase, user, roles string) (int, string) {
	req := httptest.NewRequest(r.method, r.path, bytes.NewReader([]byte(r.body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User", user)
	req.Header.Set("X-Roles", roles)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func boolPtr(b bool) *bool { return &b }
