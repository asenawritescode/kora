package analytics

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func TestBackfillStateDistributionUsesDialectTextCast(t *testing.T) {
	for _, tc := range []struct {
		driver string
		expr   string
	}{
		{driver: "postgres", expr: "('state=' || CAST(doc_status AS TEXT))"},
		{driver: "mysql", expr: "CONCAT('state=', CAST(doc_status AS CHAR))"},
		{driver: "libsql", expr: "('state=' || CAST(doc_status AS TEXT))"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			mock.ExpectExec(regexp.QuoteMeta(tc.expr)).WillReturnResult(sqlmock.NewResult(0, 1))
			metric := &Metric{Name: "state", Type: MetricStateDistribution, DocType: "Task"}
			err = backfillMetric(database, db.Resolve(tc.driver), "tenant", &doctype.DocType{Name: "Task"}, metric, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("backfillMetric: %v (SQL expression %q)", err, tc.expr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackfillGroupedDimensionsArePortable(t *testing.T) {
	for _, tc := range []struct {
		driver string
		metric Metric
		part   string
	}{
		{driver: "postgres", metric: Metric{Type: MetricCountByField, Field: "status"}, part: "('status=' ||"},
		{driver: "mysql", metric: Metric{Type: MetricCountByField, Field: "status"}, part: "CONCAT('status=',"},
		{driver: "libsql", metric: Metric{Type: MetricCountByField, Field: "status"}, part: "('status=' ||"},
		{driver: "postgres", metric: Metric{Type: MetricSumByField, Field: "amount", GroupByField: "region"}, part: "('region=' ||"},
		{driver: "mysql", metric: Metric{Type: MetricSumByField, Field: "amount", GroupByField: "region"}, part: "CONCAT('region=',"},
		{driver: "libsql", metric: Metric{Type: MetricSumByField, Field: "amount", GroupByField: "region"}, part: "('region=' ||"},
	} {
		t.Run(tc.driver+"/"+string(tc.metric.Type), func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			mock.ExpectExec(".*" + regexp.QuoteMeta(tc.part) + ".*").WillReturnResult(sqlmock.NewResult(0, 1))
			metric := tc.metric
			metric.Name, metric.DocType = "grouped", "Task"
			err = backfillMetric(database, db.Resolve(tc.driver), "tenant", &doctype.DocType{Name: "Task"}, &metric, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatalf("backfillMetric: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEntityOwnedBackfillPartitionsMetricsByLegalEntity(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec(`INSERT INTO _kora_analytics_daily .*'legal_entity', \"legal_entity_id\".*GROUP BY DATE\(\"creation\"\), \"legal_entity_id\"`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	dt := &doctype.DocType{Name: "Invoice", BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id"}
	metric := &Metric{Name: "count", Type: MetricCount, DocType: dt.Name}
	if err := backfillMetric(database, db.Resolve("postgres"), "tenant-a", dt, metric, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("entity-owned backfill: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillRollsBackDeletesAndEarlierMetricsOnWriteFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	mock.ExpectExec("CREATE TABLE IF NOT EXISTS _kora_analytics_bootstrap").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT version FROM _kora_analytics_bootstrap WHERE id = 1").
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(analyticsBootstrapVersion))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO _kora_analytics_rebuild_lock`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_analytics_rebuild_lock SET fence = fence \+ 1 WHERE site = \?`).
		WithArgs("tenant").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT doctype, from_date, last_source_sequence, scope_kind, legal_entity_id FROM _kora_analytics_rebuild_checkpoint WHERE site = \?`).
		WithArgs("tenant").WillReturnRows(sqlmock.NewRows([]string{"doctype", "from_date", "last_source_sequence", "scope_kind", "legal_entity_id"}))
	mock.ExpectQuery(`SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1`).
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(12))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_analytics_daily WHERE site = ? AND doctype = ? AND scope_kind = 'tenant' AND legal_entity_id = '' AND date >= ?")).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_analytics_monthly WHERE site = ? AND doctype = ? AND scope_kind = 'tenant' AND legal_entity_id = '' AND month >= ?")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_analytics_daily").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_analytics_daily").WillReturnError(errors.New("simulated metric write failure"))
	mock.ExpectRollback()

	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", IsSubmittable: true})
	count, err := Backfill(database, db.Resolve("mysql"), "tenant", registry, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err == nil {
		t.Fatal("expected rebuild write failure")
	}
	if count != 1 {
		t.Fatalf("completed metric count = %d, want first staged metric only", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
