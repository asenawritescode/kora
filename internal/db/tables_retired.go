package db

// retiredAnalyticsTables are the counter, rollup and rebuild tables of the
// analytics engine that Engine deleted. Tenant bootstrap drops them on MySQL
// and PostgreSQL. Projection storage (_kora_analytics_rebuild_lock and
// _kora_analytics_module_projection) is not listed: it stays in use.
var retiredAnalyticsTables = []string{
	"_kora_analytics_bootstrap",
	"_kora_analytics_metric",
	"_kora_analytics_daily",
	"_kora_analytics_monthly",
	"_kora_analytics_events",
	"_kora_analytics_event_receipt",
	"_kora_analytics_workflow",
	"_kora_analytics_rebuild_checkpoint",
	"_kora_analytics_rebuild_job",
	"_kora_analytics_rebuild_lease",
}

// RetiredAnalyticsDropDDL returns idempotent DROP TABLE IF EXISTS statements
// for the retired analytics tables, quoted for dialect.
func RetiredAnalyticsDropDDL(dialect Dialect) []string {
	statements := make([]string, 0, len(retiredAnalyticsTables))
	for _, table := range retiredAnalyticsTables {
		statements = append(statements, "DROP TABLE IF EXISTS "+dialect.QuoteIdent(table))
	}
	return statements
}
