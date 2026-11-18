//go:build integration

package orm

import (
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestReserveNumberBlockRecordsDisjointOpenBlocksForTwoTills reserves a block
// for each of two till sessions at the same time and checks the recorded rows.
func TestReserveNumberBlockRecordsDisjointOpenBlocksForTwoTills(t *testing.T) {
	cases := []struct {
		name    string
		driver  string
		env     string
		dialect db.Dialect
	}{
		{"mysql", "mysql", "KORA_ORM_DISPOSABLE_MYSQL_DSN", &db.MySQLDialect{}},
		{"postgres", "postgres", "KORA_ORM_DISPOSABLE_POSTGRES_DSN", &db.PostgresDialect{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.name)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			var ddl []string
			for _, stmt := range tc.dialect.SystemTableSQL() {
				if strings.Contains(stmt, "_kora_naming_series") || strings.Contains(stmt, "_kora_number_block") {
					ddl = append(ddl, stmt)
				}
			}
			if err := tc.dialect.ExecuteBatch(database.DB, ddl); err != nil {
				t.Fatal("create counter and block tables:", err)
			}
			if _, err := database.DB.Exec("CREATE TABLE " + tc.dialect.QuoteIdent("tabSale") + " (name VARCHAR(140) PRIMARY KEY)"); err != nil {
				t.Fatal("create record table:", err)
			}

			sale := &doctype.DocType{Name: "Sale", NamingSeries: "SALE-.YYYY.-.#####"}
			at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
			owners := []string{"till-session-1", "till-session-2"}
			blocks := make([]NumberBlock, len(owners))
			errs := make([]error, len(owners))
			var wg sync.WaitGroup
			for i, owner := range owners {
				wg.Add(1)
				go func() {
					defer wg.Done()
					blocks[i], errs[i] = reserveAndCommit(database.DB, tc.dialect, sale, owner, at)
				}()
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}

			if blocks[0].Last >= blocks[1].First && blocks[1].Last >= blocks[0].First {
				t.Fatalf("blocks overlap: %+v and %+v", blocks[0], blocks[1])
			}
			for _, b := range blocks {
				if b.State != BlockOpen || b.Prefix != "SALE-2026-" || b.Last-b.First+1 != 50 {
					t.Fatalf("unexpected block %+v", b)
				}
			}
			var rows int
			if err := database.DB.QueryRow("SELECT COUNT(*) FROM _kora_number_block WHERE state = 'open'").Scan(&rows); err != nil {
				t.Fatal("count recorded blocks:", err)
			}
			if rows != len(owners) {
				t.Fatalf("recorded %d open blocks, want %d", rows, len(owners))
			}
		})
	}
}

// reserveAndCommit reserves a block for one till session in its own transaction.
func reserveAndCommit(database *sql.DB, dialect db.Dialect, dt *doctype.DocType, owner string, at time.Time) (NumberBlock, error) {
	tx, err := database.Begin()
	if err != nil {
		return NumberBlock{}, err
	}
	defer tx.Rollback()
	block, err := ReserveNumberBlock(tx, dialect, dt, NumberBlockRequest{Owner: owner, Size: 50, At: at})
	if err != nil {
		return NumberBlock{}, err
	}
	return block, tx.Commit()
}
