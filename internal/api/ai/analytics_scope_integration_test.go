//go:build integration

package ai

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// The insight rollup is keyed by legal entity. A caller with an exact grant
// for entity A must see only A's total, never the sum across entities.
func TestAIInsightRollupIsBoundToLegalEntityScope(t *testing.T) {
	cases := []struct {
		name, driver, envVar, dialect string
	}{
		{name: "mysql", driver: "mysql", envVar: "KORA_TEST_DSN", dialect: "mysql"},
		{name: "postgres", driver: "postgres", envVar: "KORA_API_LIVE_POSTGRES_DSN", dialect: "postgres"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run the rollup scope test against %s", tc.envVar, tc.name)
			}
			const tenant = "scope-site"
			if probe, err := sql.Open(tc.driver, dsn); err == nil {
				pingErr := probe.Ping()
				_ = probe.Close()
				if pingErr != nil {
					t.Skipf("%s unavailable: %v", tc.name, pingErr)
				}
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := kdb.Resolve(tc.dialect)
			if err := analytics.BootstrapTables(context.Background(), database.DB, dialect); err != nil {
				t.Fatal("bootstrap analytics tables:", err)
			}
			today := time.Now().UTC().Format("2006-01-02")
			insert := kdb.Rebind(dialect, `INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, scope_kind, legal_entity_id, value) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
			rows := []struct {
				kind   string
				entity string
				value  float64
			}{
				{kind: string(contract.ScopeKindLegalEntity), entity: "entity-a", value: 3},
				{kind: string(contract.ScopeKindLegalEntity), entity: "entity-b", value: 5},
				{kind: string(contract.ScopeKindTenant), entity: "", value: 8},
			}
			for _, row := range rows {
				if _, err := database.DB.Exec(insert, tenant, "Sale", "sale_total", "", today, row.kind, row.entity, row.value); err != nil {
					t.Fatal("insert rollup row:", err)
				}
			}

			registry := doctype.NewRegistry()
			registry.Register(&doctype.DocType{Name: "Sale", BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id"})
			grantA := []policy.Grant{{Resource: "Sale", Operation: "read", Scope: contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}}}
			execution := analyticsTestExecution(t, tenant, []string{"Cashier"}, grantA)
			tx := &orm.TxManager{DB: database.DB, Dialect: dialect, BusinessScope: contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}}

			out := executeAnalyticsInsights(tx, registry, "Sale", tenant, execution)
			if !strings.Contains(out, "Total: 3\n") {
				t.Fatalf("entity A insight should total only entity A (3):\n%s", out)
			}
			if strings.Contains(out, "Total: 8") || strings.Contains(out, "Total: 5") {
				t.Fatalf("entity A insight summed rows of another scope:\n%s", out)
			}
		})
	}
}
