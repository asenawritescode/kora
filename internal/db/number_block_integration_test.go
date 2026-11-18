//go:build integration

package db

import (
	"database/sql"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/internal/disposabledb"
)

// TestReserveSeriesRangeGivesDisjointRangesToTwoTills has two tills reserve
// blocks at the same time. Every number must be issued once, and the blocks
// together must cover one unbroken run.
func TestReserveSeriesRangeGivesDisjointRangesToTwoTills(t *testing.T) {
	cases := []struct {
		name    string
		driver  string
		env     string
		dialect Dialect
	}{
		{"mysql", "mysql", "KORA_DB_DISPOSABLE_MYSQL_DSN", &MySQLDialect{}},
		{"postgres", "postgres", "KORA_DB_DISPOSABLE_POSTGRES_DSN", &PostgresDialect{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.name)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			var counter []string
			for _, stmt := range tc.dialect.SystemTableSQL() {
				if strings.Contains(stmt, "_kora_naming_series") {
					counter = append(counter, stmt)
				}
			}
			if err := tc.dialect.ExecuteBatch(database.DB, counter); err != nil {
				t.Fatal("create naming counter table:", err)
			}

			const tills, rounds, size = 2, 2, 50
			type block struct{ first, last int64 }
			blocks := make(chan block, tills*rounds)
			errs := make(chan error, tills*rounds)
			var wg sync.WaitGroup
			for range tills {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range rounds {
						first, last, err := reserveOne(database.DB, tc.dialect, size)
						if err != nil {
							errs <- err
							return
						}
						blocks <- block{first, last}
					}
				}()
			}
			wg.Wait()
			close(blocks)
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}

			var all []int64
			for b := range blocks {
				if b.last-b.first+1 != size {
					t.Fatalf("block %d..%d has the wrong size", b.first, b.last)
				}
				for n := b.first; n <= b.last; n++ {
					all = append(all, n)
				}
			}
			slices.Sort(all)
			for i, n := range all {
				if n != int64(i+1) {
					t.Fatalf("issued numbers are not one unbroken run: position %d holds %d", i, n)
				}
			}
		})
	}
}

// reserveOne reserves one block in its own transaction, as a till would.
func reserveOne(database *sql.DB, dialect Dialect, size int64) (int64, int64, error) {
	tx, err := database.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	first, last, err := dialect.ReserveSeriesRange(tx, "Sale", "SALE-2026-", size, func() (int64, error) { return 0, nil })
	if err != nil {
		return 0, 0, err
	}
	return first, last, tx.Commit()
}
