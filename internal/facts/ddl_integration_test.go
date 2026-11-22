//go:build integration

package facts

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestFactTablesAcceptMappedRows creates the fact tables in a fresh database and writes
// the rows Map produces for one Sale with two lines, against the real dialect's types.
func TestFactTablesAcceptMappedRows(t *testing.T) {
	tests := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			dt := saleFacts()
			for _, stmt := range DDL(dialect, dt) {
				if _, err := database.DB.Exec(stmt); err != nil {
					t.Fatalf("DDL %q: %v", stmt, err)
				}
			}
			change := Change{
				RecordID: "SAL-1", Owner: "u1", Sequence: 7,
				OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
				Data: map[string]any{"posting_date": "2026-11-18", "total": "100.50", "status": "Paid", "register": "R1",
					"items": []any{line("L1", "I1", "40.25"), line("L2", "I2", "60.25")}},
			}
			head, lines, err := Map(dt, change, tenanttime.Default())
			if err != nil {
				t.Fatalf("map: %v", err)
			}
			rows := append([]Row{head}, lines...)
			for _, r := range rows {
				table := dt.Facts.TableName(dt)
				if r.LineID != "" {
					table = dt.Facts.LineTableName(dt)
				}
				insertRow(t, database.DB, dialect, table, r)
			}
			var n int
			var sum string
			query := fmt.Sprintf("SELECT COUNT(*), SUM(%s) FROM %s", dialect.QuoteIdent("line_amount"), dialect.QuoteIdent(dt.Facts.LineTableName(dt)))
			if err := database.DB.QueryRowContext(context.Background(), query).Scan(&n, &sum); err != nil {
				t.Fatal(err)
			}
			if n != 2 || !strings.HasPrefix(sum, "100.5") {
				t.Fatalf("line table has %d rows, sum %q; want 2 rows, sum 100.5", n, sum)
			}
		})
	}
}

// insertRow writes one fact row with its columns in sorted order.
func insertRow(t *testing.T, conn *sql.DB, dialect db.Dialect, table string, r Row) {
	t.Helper()
	values := map[string]any{
		"record_id": r.RecordID, "line_id": r.LineID, "legal_entity": r.LegalEntity, "owner": r.Owner,
		"business_date": r.BusinessDate.String(), "occurred_at": r.OccurredAt, "status": r.Status,
		"source_sequence": r.SourceSequence, "deleted": r.Deleted,
	}
	for k, v := range r.Measures {
		values[k] = v.String()
	}
	for k, v := range r.Dimensions {
		values[k] = v
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	cols := make([]string, len(names))
	args := make([]any, len(names))
	marks := make([]string, len(names))
	for i, name := range names {
		cols[i] = dialect.QuoteIdent(name)
		args[i] = values[name]
		marks[i] = dialect.Placeholder(i + 1)
	}
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", dialect.QuoteIdent(table), strings.Join(cols, ", "), strings.Join(marks, ", "))
	if _, err := conn.Exec(stmt, args...); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}
