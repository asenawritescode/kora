//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestSiteSettingsUpsertAndClockRead PUTs settings through the handler, then
// reads them back with tenanttime.Load and the GET path, on a disposable
// database for each dialect.
func TestSiteSettingsUpsertAndClockRead(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_SITE_SETTINGS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_SITE_SETTINGS_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database := disposabledb.Open(t, tc.dialect, dsn)
			for _, ddl := range createTableStatements(kdb.Resolve(tc.dialect).SystemTableSQL()) {
				if _, err := database.DB.Exec(ddl); err != nil {
					t.Fatalf("create system tables: %v", err)
				}
			}
			h := NewHandler(doctype.NewRegistry(), nil)
			put := func(body string) {
				t.Helper()
				c, w := settingsContext(database.DB, tc.dialect, body)
				h.HandleSiteSettingsUpdate(c)
				if w.Code != http.StatusOK {
					t.Fatalf("PUT %s = %d: %s", body, w.Code, w.Body.String())
				}
			}
			put(`{"currency":"UGX","time_zone":"Europe/London","business_day_cutoff":"03:00"}`)
			// A partial PUT of one field must upsert that row and leave the rest.
			put(`{"time_zone":"Africa/Nairobi"}`)

			c, _ := settingsContext(database.DB, tc.dialect, "")
			clock, err := tenanttime.Load(context.Background(), database.DB, kdb.Resolve(tc.dialect), "it-site")
			if err != nil {
				t.Fatal(err)
			}
			if clock.Location.String() != "Africa/Nairobi" || clock.Cutoff != 3*time.Hour {
				t.Fatalf("tenanttime.Load = %s %v, want Africa/Nairobi 3h0m0s", clock.Location, clock.Cutoff)
			}

			got, err := h.readSiteSettings(c)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"currency": "UGX", "currency_precision": 0, "time_zone": "Africa/Nairobi", "business_day_cutoff": "03:00"}
			for key, value := range want {
				if got[key] != value {
					t.Fatalf("settings[%s] = %v, want %v (all: %v)", key, got[key], value, got)
				}
			}
		})
	}
}

func settingsContext(db any, dialect, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("site_db", db)
	c.Set("site_db_type", dialect)
	c.Set("site_name", "it-site")
	c.Set("site_registry", doctype.NewRegistry())
	exec, err := policy.NewVerifiedExecution(contract.TenantID("it-tenant"), contract.ActorContext{
		PrincipalID: "admin", PrincipalType: contract.PrincipalHuman, SubjectUserID: "admin",
		Site: "it-site", Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now(),
	})
	if err != nil {
		panic(err)
	}
	c.Set("policy_execution", exec)
	return c, w
}

// createTableStatements keeps the idempotent CREATE TABLE statements. The
// system DDL also carries column ALTERs that fail on a second run.
func createTableStatements(ddl []string) []string {
	var creates []string
	for _, s := range ddl {
		if strings.HasPrefix(strings.TrimSpace(s), "CREATE TABLE IF NOT EXISTS") {
			creates = append(creates, s)
		}
	}
	return creates
}
