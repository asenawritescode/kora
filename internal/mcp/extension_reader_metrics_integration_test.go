//go:build integration

package mcp

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/lib/pq"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestKoraMCPServesMetricsToAuthenticatedExtension runs the server the way `kora mcp` builds it for an extension
// credential. It lists the metrics tools, answers them through the real metrics handlers as the extension, and
// refuses a credential the tenant does not hold. Each run gets its own database with the system tables, dropped
// when the test ends. The DSNs must point to disposable services.
func TestKoraMCPServesMetricsToAuthenticatedExtension(t *testing.T) {
	for _, tc := range []struct{ name, driver, env string }{
		{"mysql", "mysql", "KORA_MCP_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_MCP_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			ctx := context.Background()
			database := disposabledb.Open(t, tc.driver, dsn).DB
			prepareExtensionMetricsFixture(t, ctx, database, tc.driver)

			dialect := kdb.Resolve(tc.driver)
			registry := doctype.NewRegistry()
			registry.Register(&doctype.DocType{Name: "Task", SortField: "name", SortOrder: "ASC", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}})
			reader, err := NewExtensionReader(ExtensionReaderOptions{Database: database, Dialect: dialect, Registry: registry, TenantID: contract.TenantID("stable-tenant"), Site: "tenant.example", Token: "test-token"})
			if err != nil {
				t.Fatal("create extension reader:", err)
			}
			server := NewReadOnlyWithExecutor(registry, "tenant.example", reader.Execute, WithMetrics(reader.Metrics, reader.InsightsAllowed))
			session := connectServer(t, server)

			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal("list tools:", err)
			}
			published := map[string]bool{}
			for _, tool := range tools.Tools {
				published[tool.Name] = true
			}
			for _, name := range []string{"list_metrics", "query_metrics", "drill_metrics"} {
				if !published[name] {
					t.Fatalf("tool %q is not published by kora mcp for an extension", name)
				}
			}

			list := callTool(t, session, "list_metrics", map[string]any{})
			if strings.HasPrefix(list, "Error") {
				t.Fatalf("list_metrics for the extension failed: %s", list)
			}
			query := callTool(t, session, "query_metrics", map[string]any{
				"metrics": []string{"items_sold"}, "period": map[string]any{"preset": "today"}, "grain": "range",
			})
			if !strings.Contains(query, `"error"`) || !strings.Contains(query, "items_sold") {
				t.Fatalf("query_metrics for an unknown metric = %s, want the handler's refusal naming it", query)
			}

			stranger, err := NewExtensionReader(ExtensionReaderOptions{Database: database, Dialect: dialect, Registry: registry, TenantID: contract.TenantID("stable-tenant"), Site: "tenant.example", Token: "not-a-held-token"})
			if err != nil {
				t.Fatal("create reader for a credential the tenant does not hold:", err)
			}
			refused := NewReadOnlyWithExecutor(registry, "tenant.example", stranger.Execute, WithMetrics(stranger.Metrics, stranger.InsightsAllowed))
			result, err := connectServer(t, refused).CallTool(ctx, &sdkmcp.CallToolParams{
				Name: "query_metrics", Arguments: map[string]any{"metrics": []string{"items_sold"}},
			})
			if err == nil && (result == nil || !result.IsError) {
				t.Fatalf("query_metrics with a credential the tenant does not hold was answered: %+v", result)
			}
		})
	}
}

// prepareExtensionMetricsFixture creates the system tables and one extension holding the test credential. The site
// has no active configuration, so the metrics catalog is empty.
func prepareExtensionMetricsFixture(t *testing.T, ctx context.Context, database *sql.DB, driver string) {
	t.Helper()
	if err := site.BootstrapSystemTables(ctx, database, kdb.Resolve(driver)); err != nil {
		t.Fatal("create system tables:", err)
	}
	insert := `INSERT INTO _kora_extension (name, site, endpoint_url, secret, api_permissions, access_token_hash, is_active) VALUES (?, ?, '', '', ?, ?, ?)`
	if _, err := database.ExecContext(ctx, kdb.Rebind(kdb.Resolve(driver), insert), "reader", "tenant.example",
		`[{"doctype":"Task","read":true,"if_owner":true}]`, "4c5dc9b7708905f77f5e5d16316b5dfb425e68cb326dcd55a860e90a7707031e", true); err != nil {
		t.Fatalf("prepare extension fixture: %v", err)
	}
}

// callTool calls one tool and returns its text. A failed call returns its error text, so refusals read the same as
// the Error lines the chat sees.
func callTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "Error: " + err.Error()
	}
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*sdkmcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	if result.IsError {
		return "Error: " + b.String()
	}
	return b.String()
}
