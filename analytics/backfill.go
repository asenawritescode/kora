package analytics

import (
	"database/sql"
	"fmt"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// Backfill rebuilds analytics from the source tables for a site. It is safe to
// call repeatedly: rollup keys are upserted and stale rows in the requested
// scope are removed before regeneration.
func Backfill(db *sql.DB, dialect kdb.Dialect, siteName string, registry *doctype.Registry, from time.Time, doctypeName string) (int, error) {
	if db == nil || registry == nil {
		return 0, fmt.Errorf("analytics backfill requires database and registry")
	}
	if err := BootstrapTables(db, dialect); err != nil {
		return 0, err
	}
	total := 0
	for _, name := range registry.Names() {
		if doctypeName != "" && name != doctypeName {
			continue
		}
		dt := registry.Get(name)
		if dt == nil {
			continue
		}
		for _, table := range []string{"_kora_analytics_daily", "_kora_analytics_monthly"} {
			if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE site = ? AND doctype = ? AND %s >= ?", table, map[string]string{"_kora_analytics_daily": "date", "_kora_analytics_monthly": "month"}[table]), siteName, dt.Name, from.Format("2006-01-02")); err != nil {
				return total, err
			}
		}
		for _, metric := range GenerateMetrics(dt) {
			if err := backfillMetric(db, dialect, siteName, dt, metric, from); err != nil {
				return total, fmt.Errorf("%s/%s: %w", dt.Name, metric.Name, err)
			}
			total++
		}
	}
	// Materialize monthly buckets immediately so wide report ranges do not wait
	// for the scheduled worker.
	monthExpr := "DATE_FORMAT(date, '%Y-%m-01')"
	if dialect.DriverName() != "mysql" {
		monthExpr = "date_trunc('month', date)"
	}
	upsert := dialect.UpsertClause([]string{"site", "doctype", "metric", "dimension", "month"}, []string{"value"})
	_, err := db.Exec(fmt.Sprintf(`INSERT INTO _kora_analytics_monthly (site, doctype, metric, dimension, month, value)
		SELECT site, doctype, metric, dimension, %s, SUM(value) FROM _kora_analytics_daily
		WHERE site = ? AND date >= ? GROUP BY site, doctype, metric, dimension, %s %s`, monthExpr, monthExpr, upsert), siteName, from.Format("2006-01-02"))
	if err != nil {
		return total, err
	}
	return total, nil
}

func backfillMetric(db *sql.DB, dialect kdb.Dialect, siteName string, dt *doctype.DocType, m *Metric, from time.Time) error {
	q, table := dialect.QuoteIdent, dt.TableName()
	timeColumn := q("creation")
	if m.TimeField != "" {
		timeColumn = q(m.TimeField)
	}
	bucket := fmt.Sprintf("DATE(%s)", timeColumn)
	upsert := dialect.UpsertClause([]string{"site", "doctype", "metric", "dimension", "date"}, []string{"value"})
	args := []any{siteName, dt.Name, m.Name, from}
	switch m.Type {
	case MetricCount, MetricCountByTime:
		_, err := db.Exec(fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, '', %s, COUNT(*) FROM %s WHERE %s >= ? GROUP BY %s %s`, bucket, table, timeColumn, bucket, upsert), args...)
		return err
	case MetricCountByField:
		col := q(m.Field)
		_, err := db.Exec(fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, CONCAT('%s=', %s), %s, COUNT(*) FROM %s WHERE %s >= ? AND %s IS NOT NULL AND %s != '' GROUP BY %s, %s %s`, m.Field, col, bucket, table, timeColumn, col, col, col, bucket, upsert), args...)
		return err
	case MetricSum:
		col := q(m.Field)
		_, err := db.Exec(fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, '', %s, COALESCE(SUM(%s), 0) FROM %s WHERE %s >= ? GROUP BY %s %s`, bucket, col, table, timeColumn, bucket, upsert), args...)
		return err
	case MetricSumByField:
		col, group := q(m.Field), q(m.GroupByField)
		_, err := db.Exec(fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, CONCAT('%s=', COALESCE(%s, '')), %s, COALESCE(SUM(%s), 0) FROM %s WHERE %s >= ? AND %s IS NOT NULL AND %s IS NOT NULL AND %s != '' GROUP BY %s, %s %s`, m.GroupByField, group, bucket, col, table, timeColumn, col, group, group, group, bucket, upsert), args...)
		return err
	case MetricStateDistribution:
		_, err := db.Exec(fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, CONCAT('state=', CAST(doc_status AS CHAR)), %s, COUNT(*) FROM %s WHERE %s >= ? GROUP BY doc_status, %s %s`, bucket, table, timeColumn, bucket, upsert), args...)
		return err
	}
	return nil
}
