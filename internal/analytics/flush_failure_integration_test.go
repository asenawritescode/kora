//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// verifyFlushFailureRetry exercises the SQL/WAL boundary against a real
// database. A transient database write failure must roll back both the event
// receipt and rollup effect, leave the event in the durable WAL, and permit a
// retry to commit exactly one effect before acknowledging that WAL record.
func verifyFlushFailureRetry(t *testing.T, database *sql.DB, dialect db.QueryDialect, site string, registry *doctype.Registry, installFailure, removeFailure func() error) {
	t.Helper()
	bus := NewChannelBus(8, t.TempDir()+"/analytics-wal")
	t.Cleanup(func() { _ = bus.Close() })
	worker := NewWorker(bus, database, dialect, registry, site, LoadConfig())
	event := ChangeEvent{
		ID:        "flush-retry-" + site,
		Site:      site,
		Doctype:   registry.Names()[0],
		DocName:   "source-1",
		Operation: EventInsert,
		Timestamp: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		Data:      map[string]any{"name": "source-1"},
	}
	if err := bus.Publish(event); err != nil {
		t.Fatal("persist source event to WAL:", err)
	}
	worker.process(event)

	query := db.Rebind(dialect, `SELECT value FROM _kora_analytics_daily WHERE site = ? AND doctype = ? AND metric = ? AND dimension = '' AND date = ?`)
	var before sql.NullFloat64
	err := database.QueryRow(query, site, event.Doctype, metricName(event.Doctype)+"_count", "2026-10-14").Scan(&before)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("read initial rollup:", err)
	}
	if err := installFailure(); err != nil {
		t.Fatal("install transient analytics write failure:", err)
	}
	failureInstalled := true
	t.Cleanup(func() {
		if failureInstalled {
			if err := removeFailure(); err != nil {
				t.Errorf("remove transient analytics write failure: %v", err)
			}
		}
	})
	if err := worker.Flush(); err == nil {
		t.Fatal("analytics flush succeeded while writes were rejected")
	}
	if err := removeFailure(); err != nil {
		t.Fatal("remove transient analytics write failure:", err)
	}
	failureInstalled = false

	var receipts int
	if err := database.QueryRow(db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_analytics_event_receipt WHERE site = ? AND event_id = ?`), site, event.ID).Scan(&receipts); err != nil {
		t.Fatal("read receipt after failed flush:", err)
	}
	if receipts != 0 {
		t.Fatalf("receipts after failed flush = %d, want 0", receipts)
	}
	var retryable []string
	if _, err := bus.DrainWAL(func(event ChangeEvent) { retryable = append(retryable, event.ID) }); err != nil {
		t.Fatal("read WAL after failed flush:", err)
	}
	if len(retryable) != 1 || retryable[0] != event.ID {
		t.Fatalf("WAL after failed flush = %v, want event %q", retryable, event.ID)
	}
	var afterFailure sql.NullFloat64
	err = database.QueryRow(query, site, event.Doctype, metricName(event.Doctype)+"_count", "2026-10-14").Scan(&afterFailure)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("read rollup after failed flush:", err)
	}
	if before.Valid != afterFailure.Valid || before.Valid && before.Float64 != afterFailure.Float64 {
		t.Fatalf("rollup after failed flush = %v (valid %v), prior value = %v (valid %v)", afterFailure.Float64, afterFailure.Valid, before.Float64, before.Valid)
	}

	if err := worker.Flush(); err != nil {
		t.Fatal("retry analytics flush:", err)
	}
	var afterRetry float64
	if err := database.QueryRow(query, site, event.Doctype, metricName(event.Doctype)+"_count", "2026-10-14").Scan(&afterRetry); err != nil {
		t.Fatal("read rollup after retry:", err)
	}
	want := 1.0
	if before.Valid {
		want += before.Float64
	}
	if afterRetry != want {
		t.Fatalf("rollup after retry = %v, want %v", afterRetry, want)
	}
	if err := worker.Flush(); err != nil {
		t.Fatal("flush after retry commit:", err)
	}
	var afterDuplicateFlush float64
	if err := database.QueryRow(query, site, event.Doctype, metricName(event.Doctype)+"_count", "2026-10-14").Scan(&afterDuplicateFlush); err != nil {
		t.Fatal("read rollup after empty duplicate flush:", err)
	}
	if afterDuplicateFlush != afterRetry {
		t.Fatalf("rollup after duplicate flush = %v, want %v", afterDuplicateFlush, afterRetry)
	}
	var remaining []string
	if _, err := bus.DrainWAL(func(event ChangeEvent) { remaining = append(remaining, event.ID) }); err != nil {
		t.Fatal("read WAL after successful retry:", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("WAL after successful retry retains events %v", remaining)
	}
	if err := database.QueryRow(db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_analytics_event_receipt WHERE site = ? AND event_id = ?`), site, event.ID).Scan(&receipts); err != nil {
		t.Fatal("read receipt after retry:", err)
	}
	if receipts != 1 {
		t.Fatalf("receipts after retry = %d, want 1", receipts)
	}

	// Exercise the monthly upsert against the same dialect. PostgreSQL needs an
	// explicit target-table reference when incrementing a conflicted value.
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	monthKey := monthStart[:7] + "-01"
	for _, seed := range []struct{ query, bucket string }{
		{query: `INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) VALUES (?, ?, ?, '', ?, 1)`, bucket: monthStart},
		{query: `INSERT INTO _kora_analytics_monthly (site, doctype, metric, dimension, month, value) VALUES (?, ?, ?, '', ?, 4)`, bucket: monthKey},
	} {
		if _, err := database.Exec(db.Rebind(dialect, seed.query), site, event.Doctype, "flush_rollup_probe", seed.bucket); err != nil {
			t.Fatal("seed monthly rollup increment:", err)
		}
	}
	monthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	var dailySum sql.NullFloat64
	if err := database.QueryRow(db.Rebind(dialect, `SELECT SUM(value) FROM _kora_analytics_daily WHERE site = ? AND metric = ? AND date >= ? AND date < ?`), site, "flush_rollup_probe", monthStart, monthEnd).Scan(&dailySum); err != nil {
		t.Fatal("read daily input for monthly rollup:", err)
	}
	if !dailySum.Valid || dailySum.Float64 != 1 {
		t.Fatalf("monthly rollup input sum = %v (valid %v), want 1", dailySum.Float64, dailySum.Valid)
	}
	worker.rollupMonthly(context.Background())
	var monthlyValue float64
	if err := database.QueryRow(db.Rebind(dialect, `SELECT value FROM _kora_analytics_monthly WHERE site = ? AND doctype = ? AND metric = ? AND dimension = '' AND month = ?`), site, event.Doctype, "flush_rollup_probe", monthKey).Scan(&monthlyValue); err != nil {
		t.Fatal("read monthly rollup increment:", err)
	}
	if monthlyValue != 5 {
		rows, queryErr := database.Query(db.Rebind(dialect, `SELECT month, value FROM _kora_analytics_monthly WHERE site = ? AND doctype = ? AND metric = ? AND dimension = ''`), site, event.Doctype, "flush_rollup_probe")
		if queryErr != nil {
			t.Fatalf("monthly upsert value = %v, want 5 (inspect query failed: %v)", monthlyValue, queryErr)
		}
		var actual []string
		for rows.Next() {
			var month any
			var value float64
			if scanErr := rows.Scan(&month, &value); scanErr != nil {
				t.Fatal("scan monthly diagnostic:", scanErr)
			}
			actual = append(actual, fmt.Sprint(month)+"="+fmt.Sprint(value))
		}
		rows.Close()
		t.Fatalf("monthly upsert value = %v, want 5; month key %s, rows %v", monthlyValue, monthKey, actual)
	}
}
