package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// Backfill rebuilds analytics from the source tables for a site. It is safe to
// call repeatedly: rollup keys are upserted and stale rows in the requested
// scope are removed before regeneration.
func Backfill(db *sql.DB, dialect kdb.Dialect, siteName string, registry *doctype.Registry, from time.Time, doctypeName string) (int, error) {
	return BackfillContext(context.Background(), db, dialect, siteName, registry, from, doctypeName)
}

// BackfillContext runs the rebuild under ctx so shutdown/deadline cancellation
// can release the tenant rebuild lock and leave prior rollups intact.
func BackfillContext(ctx context.Context, db *sql.DB, dialect kdb.Dialect, siteName string, registry *doctype.Registry, from time.Time, doctypeName string) (int, error) {
	if db == nil || registry == nil {
		return 0, fmt.Errorf("analytics backfill requires database and registry")
	}
	if ctx == nil {
		return 0, fmt.Errorf("analytics backfill requires context")
	}
	utcFrom := from.UTC()
	from = time.Date(utcFrom.Year(), utcFrom.Month(), utcFrom.Day(), 0, 0, 0, 0, time.UTC)
	if err := BootstrapTables(db, dialect); err != nil {
		return 0, err
	}
	options := &sql.TxOptions{}
	if driver := dialect.DriverName(); driver == "mysql" || driver == "postgres" {
		options.Isolation = sql.LevelRepeatableRead
	}
	tx, err := db.BeginTx(ctx, options)
	if err != nil {
		return 0, fmt.Errorf("begin analytics rebuild: %w", err)
	}
	defer tx.Rollback()
	if _, err := lockRebuildStateTx(ctx, tx, dialect, siteName); err != nil {
		return 0, err
	}
	cutoff, err := currentSourceSequenceTx(ctx, tx, dialect)
	if err != nil {
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
			if _, err := tx.ExecContext(ctx, kdb.Rebind(dialect, fmt.Sprintf("DELETE FROM %s WHERE site = ? AND doctype = ? AND %s >= ?", table, map[string]string{"_kora_analytics_daily": "date", "_kora_analytics_monthly": "month"}[table])), siteName, dt.Name, from.Format("2006-01-02")); err != nil {
				return total, fmt.Errorf("delete existing %s rollups for %s: %w", table, dt.Name, err)
			}
		}
		for _, metric := range GenerateMetrics(dt) {
			if err := backfillMetricContext(ctx, tx, dialect, siteName, dt, metric, from); err != nil {
				return total, fmt.Errorf("%s/%s: %w", dt.Name, metric.Name, err)
			}
			total++
		}
	}
	// Materialize monthly buckets immediately so wide report ranges do not wait
	// for the scheduled worker.
	monthExpr := "DATE_FORMAT(date, '%Y-%m-01')"
	if dialect.DriverName() == "postgres" {
		monthExpr = "date_trunc('month', date)"
	} else if dialect.DriverName() == "libsql" {
		monthExpr = "strftime('%Y-%m-01', date)"
	}
	upsert := dialect.UpsertClause([]string{"site", "doctype", "metric", "dimension", "month"}, []string{"value"})
	_, err = tx.ExecContext(ctx, kdb.Rebind(dialect, fmt.Sprintf(`INSERT INTO _kora_analytics_monthly (site, doctype, metric, dimension, month, value)
		SELECT site, doctype, metric, dimension, %s, SUM(value) FROM _kora_analytics_daily
		WHERE site = ? AND date >= ? GROUP BY site, doctype, metric, dimension, %s %s`, monthExpr, monthExpr, upsert)), siteName, from.Format("2006-01-02"))
	if err != nil {
		return total, err
	}
	if err := saveRebuildCheckpointTx(ctx, tx, dialect, siteName, doctypeName, from.Format("2006-01-02"), cutoff); err != nil {
		return total, err
	}
	if err := tx.Commit(); err != nil {
		return total, fmt.Errorf("commit analytics rebuild: %w", err)
	}
	return total, nil
}

func backfillMetric(db kdb.Queryer, dialect kdb.Dialect, siteName string, dt *doctype.DocType, m *Metric, from time.Time) error {
	return backfillMetricWithExec(func(query string, args ...any) (sql.Result, error) {
		return db.Exec(query, args...)
	}, dialect, siteName, dt, m, from)
}

type contextExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func backfillMetricContext(ctx context.Context, database contextExecer, dialect kdb.Dialect, siteName string, dt *doctype.DocType, m *Metric, from time.Time) error {
	return backfillMetricWithExec(func(query string, args ...any) (sql.Result, error) {
		return database.ExecContext(ctx, query, args...)
	}, dialect, siteName, dt, m, from)
}

func backfillMetricWithExec(exec func(string, ...any) (sql.Result, error), dialect kdb.Dialect, siteName string, dt *doctype.DocType, m *Metric, from time.Time) error {
	q, table := dialect.QuoteIdent, dialect.QuoteIdent(dt.RawTableName())
	timeColumn := q("creation")
	if m.TimeField != "" {
		timeColumn = q(m.TimeField)
	}
	bucket := fmt.Sprintf("DATE(%s)", timeColumn)
	upsert := dialect.UpsertClause([]string{"site", "doctype", "metric", "dimension", "date"}, []string{"value"})
	args := []any{siteName, dt.Name, m.Name, from}
	switch m.Type {
	case MetricCount, MetricCountByTime:
		_, err := exec(kdb.Rebind(dialect, fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, '', %s, COUNT(*) FROM %s WHERE %s >= ? GROUP BY %s %s`, bucket, table, timeColumn, bucket, upsert)), args...)
		return err
	case MetricCountByField:
		col := q(m.Field)
		dimension := concatenateLiteral(dialect, m.Field+"=", col)
		_, err := exec(kdb.Rebind(dialect, fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, %s, %s, COUNT(*) FROM %s WHERE %s >= ? AND %s IS NOT NULL AND %s != '' GROUP BY %s, %s %s`, dimension, bucket, table, timeColumn, col, col, col, bucket, upsert)), args...)
		return err
	case MetricSum:
		col := q(m.Field)
		_, err := exec(kdb.Rebind(dialect, fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, '', %s, COALESCE(SUM(%s), 0) FROM %s WHERE %s >= ? GROUP BY %s %s`, bucket, col, table, timeColumn, bucket, upsert)), args...)
		return err
	case MetricSumByField:
		col, group := q(m.Field), q(m.GroupByField)
		dimension := concatenateLiteral(dialect, m.GroupByField+"=", "COALESCE("+group+", '')")
		_, err := exec(kdb.Rebind(dialect, fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, %s, %s, COALESCE(SUM(%s), 0) FROM %s WHERE %s >= ? AND %s IS NOT NULL AND %s IS NOT NULL AND %s != '' GROUP BY %s, %s %s`, dimension, bucket, col, table, timeColumn, col, group, group, group, bucket, upsert)), args...)
		return err
	case MetricStateDistribution:
		textType := "CHAR"
		if dialect.DriverName() == "postgres" || dialect.DriverName() == "libsql" {
			textType = "TEXT"
		}
		dimension := concatenateLiteral(dialect, "state=", fmt.Sprintf("CAST(doc_status AS %s)", textType))
		_, err := exec(kdb.Rebind(dialect, fmt.Sprintf(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) SELECT ?, ?, ?, %s, %s, COUNT(*) FROM %s WHERE %s >= ? GROUP BY doc_status, %s %s`, dimension, bucket, table, timeColumn, bucket, upsert)), args...)
		return err
	}
	return nil
}

func concatenateLiteral(dialect kdb.Dialect, prefix, expression string) string {
	prefix = strings.ReplaceAll(prefix, "'", "''")
	if dialect.DriverName() == "libsql" || dialect.DriverName() == "postgres" {
		return fmt.Sprintf("('%s' || %s)", prefix, expression)
	}
	return fmt.Sprintf("CONCAT('%s', %s)", prefix, expression)
}
