//go:build integration

package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

const rebuildSales = 2500

// TestRebuildResumesToCleanRebuild cancels a rebuild after its first chunk, expires the lease, resumes
// from the checkpoint, and checks the result equals a clean rebuild of the same source.
func TestRebuildResumesToCleanRebuild(t *testing.T) {
	tests := []struct {
		driver, envVar string
	}{
		{"mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			ctx := context.Background()
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			seedRebuildSource(t, database.DB, dialect)
			store := &JobStore{DB: database.DB, Dialect: dialect}
			rebuilder := func(cancel func()) *Rebuilder {
				return &Rebuilder{DB: database.DB, Dialect: dialect, Registry: reg, Site: "test", Store: store,
					chunkSize: 1000, afterChunk: cancel}
			}

			// First attempt: the context is cancelled after the first chunk commits.
			job, err := store.Enqueue(ctx, "Sale", Window{})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := store.Claim(ctx, "Sale", "worker-a")
			if err != nil || claimed == nil {
				t.Fatalf("claim = %+v, %v", claimed, err)
			}
			cancelCtx, cancel := context.WithCancel(ctx)
			err = rebuilder(cancel).Run(cancelCtx, claimed)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run = %v, want context.Canceled", err)
			}
			cancel()
			interrupted := mustGetJob(t, store, job.ID)
			if interrupted.Status != JobRunning || interrupted.Processed != 1000 || interrupted.Checkpoint == "" {
				t.Fatalf("after interrupt = %+v; want running with 1000 processed", interrupted)
			}

			// Simulated crash: the lease expires on the database clock, and a new worker resumes.
			expireLease(t, database.DB, dialect, job.ID)
			resumed, err := store.Claim(ctx, "Sale", "worker-b")
			if err != nil || resumed == nil || resumed.Checkpoint != interrupted.Checkpoint {
				t.Fatalf("resume claim = %+v, %v; want checkpoint %q", resumed, err, interrupted.Checkpoint)
			}
			if err = rebuilder(nil).Run(ctx, resumed); err != nil {
				t.Fatalf("resumed run: %v", err)
			}
			done := mustGetJob(t, store, job.ID)
			if done.Status != JobDone || done.Processed != rebuildSales {
				t.Fatalf("resumed job = %+v; want done with %d processed", done, rebuildSales)
			}
			resumedFacts := factSnapshot(t, database.DB, dialect)

			// Clean rebuild: drop the fact tables and rebuild the same source from scratch.
			for _, table := range []string{factHeader, factLine, dirtyTable} {
				if _, err = database.DB.ExecContext(ctx, "DROP TABLE "+dialect.QuoteIdent(table)); err != nil {
					t.Fatalf("drop %s: %v", table, err)
				}
			}
			clean, err := store.Enqueue(ctx, "Sale", Window{})
			if err != nil {
				t.Fatal(err)
			}
			claimedClean, err := store.Claim(ctx, "Sale", "worker-c")
			if err != nil || claimedClean == nil || claimedClean.ID != clean.ID {
				t.Fatalf("clean claim = %+v, %v", claimedClean, err)
			}
			if err = rebuilder(nil).Run(ctx, claimedClean); err != nil {
				t.Fatalf("clean run: %v", err)
			}
			cleanFacts := factSnapshot(t, database.DB, dialect)
			if strings.Join(resumedFacts, "\n") != strings.Join(cleanFacts, "\n") {
				t.Fatalf("resumed facts differ from clean rebuild:\nresumed: %v\nclean:   %v", resumedFacts[:3], cleanFacts[:3])
			}
			if len(cleanFacts) != rebuildSales*2 {
				t.Fatalf("snapshot has %d rows, want %d headers and %d lines", len(cleanFacts), rebuildSales, rebuildSales)
			}
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dirtyTable)+" WHERE model = ? AND business_date = ?", "Sale", "2026-11-18"); n != 1 {
				t.Fatalf("dirty markers for the rebuilt day = %d, want 1", n)
			}
		})
	}
}

// factHeader and factLine are the Sale fact tables; dirtyTable is the marker table.
const (
	factHeader = "_kora_fact_sale"
	factLine   = "_kora_fact_sale_line"
)

// seedRebuildSource creates the Sale source tables and rebuildSales records with one line each.
func seedRebuildSource(t *testing.T, database *sql.DB, dialect db.Dialect) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		"CREATE TABLE " + dialect.QuoteIdent("tabSale") + " (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, posting_date DATE NOT NULL, total DECIMAL(21,9) NOT NULL, status VARCHAR(140) NOT NULL, register VARCHAR(140) NOT NULL)",
		"CREATE TABLE " + dialect.QuoteIdent("tabSale__items") + " (name VARCHAR(140) NOT NULL PRIMARY KEY, parent VARCHAR(140) NOT NULL, parentfield VARCHAR(140) NOT NULL, item VARCHAR(140) NOT NULL, amount DECIMAL(21,9) NOT NULL)",
		"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
		"CREATE TABLE _kora_tenant_event_sequence (id INT PRIMARY KEY, last_sequence BIGINT NOT NULL DEFAULT 0)",
		"INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 41)",
	}
	for _, stmt := range stmts {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	// One transaction: autocommit makes every insert wait for its own commit.
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	sale := db.Rebind(dialect, "INSERT INTO "+dialect.QuoteIdent("tabSale")+" (name, owner, creation, posting_date, total, status, register) VALUES (?, 'u1', '2026-11-01 10:00:00', '2026-11-18', ?, 'Paid', 'R1')")
	item := db.Rebind(dialect, "INSERT INTO "+dialect.QuoteIdent("tabSale__items")+" (name, parent, parentfield, item, amount) VALUES (?, ?, 'items', ?, ?)")
	for i := 1; i <= rebuildSales; i++ {
		name := fmt.Sprintf("SAL-%05d", i)
		if _, err = tx.ExecContext(ctx, sale, name, fmt.Sprintf("%d.50", i)); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, item, "ITEM-"+name, name, "I"+name, fmt.Sprintf("%d.25", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// factSnapshot renders every fact row, header and line, in key order.
func factSnapshot(t *testing.T, database *sql.DB, dialect db.Dialect) []string {
	t.Helper()
	return append(tableSnapshot(t, database, dialect, factHeader), tableSnapshot(t, database, dialect, factLine)...)
}

// tableSnapshot renders each row of table as one sorted line. occurred_at is left out because it is the
// rebuild's own clock.
func tableSnapshot(t *testing.T, database *sql.DB, dialect db.Dialect, table string) []string {
	t.Helper()
	rows, err := database.Query("SELECT * FROM " + dialect.QuoteIdent(table) + " ORDER BY record_id, line_id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var parts []string
		for i, c := range cols {
			if c == "occurred_at" {
				continue
			}
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts = append(parts, fmt.Sprintf("%s=%v", c, v))
		}
		sort.Strings(parts)
		out = append(out, table+"|"+strings.Join(parts, ","))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustGetJob(t *testing.T, store *JobStore, id string) *Job {
	t.Helper()
	job, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// expireLease moves a job's lease into the past on the database clock.
func expireLease(t *testing.T, database *sql.DB, dialect db.Dialect, id string) {
	t.Helper()
	expr, arg := dbClock(dialect, -120)
	query := fmt.Sprintf("UPDATE %s SET lease_until = %s WHERE id = ?", dialect.QuoteIdent(jobTable), expr)
	if _, err := database.ExecContext(context.Background(), db.Rebind(dialect, query), arg, id); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, database *sql.DB, dialect db.Dialect, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRowContext(context.Background(), db.Rebind(dialect, query), args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
