package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/db"
)

func pagedConn(t *testing.T, n int) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec("CREATE TABLE `tabPurchase Request` (name TEXT, title TEXT, modified TEXT, owner TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := conn.Exec("INSERT INTO `tabPurchase Request` VALUES (?,?,?,?)", fmt.Sprintf("N%02d", i), "t", fmt.Sprintf("%02d", i), "u"); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

// A request limit below the definition's page_limit sizes the page; the cursor
// and the has-more signal must stay correct at the boundaries (#335).
func TestRunRequestLimitBelowPageLimit(t *testing.T) {
	tests := []struct {
		name      string
		rows      int
		limit     int
		wantRows  int
		wantMore  bool
		wantPages int
	}{
		{"empty", 0, 1, 0, false, 1},
		{"one row, limit 1: rows == limit", 1, 1, 1, false, 1},
		{"two rows, limit 1: rows == limit+1", 2, 1, 1, true, 2},
		{"five rows, limit 1", 5, 1, 1, true, 5},
		{"five rows, limit 2 pages 2,2,1", 5, 2, 2, true, 3},
		{"six rows, limit 3: exact multiple", 6, 3, 3, true, 2},
		{"page_limit-1 with more rows", 9, 4, 4, true, 3},
		{"limit == page_limit", 9, 5, 5, true, 2},
		{"limit 0 means page_limit", 9, 0, 5, true, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := def(nil)
			d.Limit = 5
			conn := pagedConn(t, tc.rows)
			req := Request{FullRead: true, Limit: tc.limit}
			var names []string
			pages := 0
			for {
				pages++
				p, err := Run(context.Background(), conn, db.Resolve("libsql"), d, req)
				if err != nil {
					t.Fatal(err)
				}
				if pages == 1 && len(p.Rows) != tc.wantRows {
					t.Fatalf("first page rows = %d, want %d", len(p.Rows), tc.wantRows)
				}
				if (p.Next != nil) != (pages < tc.wantPages) {
					t.Fatalf("page %d: next = %v, want more=%v", pages, p.Next, pages < tc.wantPages)
				}
				for _, r := range p.Rows {
					names = append(names, r["name"].(string))
				}
				if p.Next == nil {
					break
				}
				if pages > tc.rows+1 {
					t.Fatal("paging does not terminate")
				}
				req.Cursor = p.Next
			}
			if pages != tc.wantPages || len(names) != tc.rows {
				t.Fatalf("pages = %d rows = %d, want %d pages, %d rows", pages, len(names), tc.wantPages, tc.rows)
			}
			seen := map[string]bool{}
			for _, n := range names {
				if seen[n] {
					t.Fatalf("row %s repeated across pages: %v", n, names)
				}
				seen[n] = true
			}
		})
	}
}

func TestBuildRequestLimit(t *testing.T) {
	d := def(nil)
	d.Limit = 5
	for _, tc := range []struct {
		name    string
		limit   int
		want    string
		wantErr bool
	}{
		{"unset uses page_limit", 0, "LIMIT 6", false},
		{"below", 2, "LIMIT 3", false},
		{"equal", 5, "LIMIT 6", false},
		{"above is a caller error", 6, "", true},
		{"negative is a caller error", -1, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, _, _, err := Build(d, db.MySQL(), Request{FullRead: true, Limit: tc.limit})
			var ie *InputError
			if tc.wantErr {
				if !errors.As(err, &ie) || ie.Field != "limit" {
					t.Fatalf("err = %v, want limit InputError", err)
				}
				return
			}
			if err != nil || !strings.HasSuffix(q, tc.want) {
				t.Fatalf("q = %q err = %v, want suffix %q", q, err, tc.want)
			}
		})
	}
}

// Aggregates cannot page, so a limit below the group count is the caller's
// mistake, not a truncated answer or a provider fault.
func TestAggregateRequestLimit(t *testing.T) {
	rows := "('S1','R2','Paid','bob',10.5),('S2','R1','Paid','al',4),('S3','R3','Paid','bob',6)"
	for _, tc := range []struct {
		name    string
		limit   int
		want    int
		wantErr bool
	}{
		{"limit == groups", 3, 3, false},
		{"limit above groups", 5, 3, false},
		{"unset", 0, 3, false},
		{"limit below groups", 2, 0, true},
		{"limit 1", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Run(context.Background(), aggConn(t, rows), db.Resolve("libsql"), aggDef(t, grouped), Request{FullRead: true, Limit: tc.limit})
			if tc.wantErr {
				var ie *InputError
				if !errors.As(err, &ie) || ie.Field != "limit" {
					t.Fatalf("err = %v, want limit InputError", err)
				}
				return
			}
			if err != nil || len(p.Rows) != tc.want {
				t.Fatalf("rows = %v err = %v", p, err)
			}
		})
	}
}
