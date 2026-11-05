//go:build integration

package api

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
)

// The published runtime is stubbed by a receipt table on the real database:
// the first claim of a row key executes, a repeat replays the stored receipt.
func TestCSVImportOperationTargetAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_API_LIVE_MYSQL_DSN"},
		{"postgres", "KORA_API_LIVE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database, dialect := openKernelDialectDatabase(t, tc.driver, dsn)
			table := fmt.Sprintf("imp_op_receipt_%x", time.Now().UnixNano())
			q := dialect.QuoteIdent
			if _, err := database.Exec("CREATE TABLE " + q(table) + " (" + q("k") + " VARCHAR(64) PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = database.Exec("DROP TABLE " + q(table)) })
			f := newImportOpFixture(t)
			f.claim = func(key string) (contract.OperationID, bool) {
				_, err := database.Exec(kdb.Rebind(dialect, "INSERT INTO "+q(table)+" ("+q("k")+") VALUES (?)"), key)
				return contract.OperationID("exec-" + key[:12]), err != nil
			}
			var csv strings.Builder
			csv.WriteString("SKU,Qty\n")
			for i := 0; i < 50; i++ {
				fmt.Fprintf(&csv, "s%d,%d\n", i, i)
			}
			spec := `{"import_id":"live","target":{"operation_ref":"` + importOpRef + `"},"mapping":{"SKU":"sku","Qty":"qty"}`
			rows := func() int { return countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+q(table)) }
			if _, dry := f.post(t, []string{"Clerk"}, spec+`,"dry_run":true}`, csv.String()); dry["executed"] != false || rows() != 0 {
				t.Fatalf("dry run: %v receipts=%d", dry, rows())
			}
			if _, out := f.post(t, []string{"Clerk"}, spec+`}`, csv.String()); out["succeeded"] != float64(50) || rows() != 50 || len(f.calls) != 50 {
				t.Fatalf("run: %v receipts=%d calls=%d", out, rows(), len(f.calls))
			}
			if _, out := f.post(t, []string{"Clerk"}, spec+`}`, csv.String()); out["succeeded"] != float64(50) || rows() != 50 || len(f.calls) != 50 {
				t.Fatalf("replay: %v receipts=%d calls=%d", out, rows(), len(f.calls))
			}
		})
	}
}
