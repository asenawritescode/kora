package query

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	_ "github.com/mattn/go-sqlite3"
)

func def(filters []Filter) *Definition {
	return &Definition{Source: "Purchase Request", Fields: []string{"name", "title"}, Limit: 2,
		Sort: []Sort{{Field: "modified", Dir: "desc"}}, Filters: filters, OwnerField: "owner",
		Input: map[string]InputSpec{"search": {Type: "string"}}}
}

func TestBuild(t *testing.T) {
	opt := Filter{Field: "status", Op: "eq", Value: "$input.status", Optional: true}
	anyG := Filter{Optional: true, Any: []Filter{{Field: "title", Op: "contains", Value: "$input.search"}, {Field: "code", Op: "eq", Value: "$input.search"}}}
	tests := []struct {
		name    string
		d       db.Dialect
		f       []Filter
		req     Request
		want    []string
		nargs   int
		wantErr bool
	}{
		{"optional skipped", db.MySQL(), []Filter{opt}, Request{Actor: "u", FullRead: true}, []string{"FROM `Purchase Request` ORDER BY `modified` DESC, `name` ASC LIMIT 3"}, 0, false},
		{"optional present", db.MySQL(), []Filter{opt}, Request{Input: map[string]any{"status": "Draft"}, FullRead: true}, []string{"WHERE `status` = ?"}, 1, false},
		{"any is OR", db.MySQL(), []Filter{anyG}, Request{Input: map[string]any{"search": "x"}, FullRead: true}, []string{"(`title` LIKE ? ESCAPE '!' OR `code` = ?)"}, 2, false},
		{"contains escapes", db.MySQL(), []Filter{anyG}, Request{Input: map[string]any{"search": "50%_!"}, FullRead: true}, []string{"LIKE"}, 2, false},
		{"owner limits to actor", db.MySQL(), nil, Request{Actor: "bob"}, []string{"`owner` = ?"}, 1, false},
		{"scope", db.MySQL(), nil, Request{FullRead: true, ScopeField: "legal_entity", ScopeValue: "LE1"}, []string{"`legal_entity` = ?"}, 1, false},
		{"postgres rebinds", db.Resolve("postgres"), []Filter{opt}, Request{Input: map[string]any{"status": "D"}, FullRead: true}, []string{"= $1"}, 1, false},
		{"required missing", db.MySQL(), []Filter{{Field: "status", Op: "eq", Value: "$input.status"}}, Request{FullRead: true}, nil, 0, true},
		{"bad cursor", db.MySQL(), nil, Request{FullRead: true, Cursor: []any{"x"}}, nil, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, args, _, err := Build(def(tc.f), tc.d, tc.req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(q, w) {
					t.Errorf("%q missing %q", q, w)
				}
			}
			if !tc.wantErr && len(args) != tc.nargs {
				t.Errorf("args=%v", args)
			}
			if tc.name == "contains escapes" && args[0] != `%50!%!_!!%` {
				t.Errorf("pattern %v", args[0])
			}
		})
	}
}

func TestRunKeysetStableUnderInsert(t *testing.T) {
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	mustExec := func(q string, a ...any) {
		if _, err := conn.Exec(q, a...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec("CREATE TABLE `Purchase Request` (name TEXT, title TEXT, modified TEXT, owner TEXT, total NUMERIC)")
	for _, r := range [][2]string{{"A", "2"}, {"B", "3"}, {"C", "1"}} {
		mustExec("INSERT INTO `Purchase Request` VALUES (?,?,?,?,?)", r[0], "t"+r[0], r[1], "u", "1.50")
	}
	d := def(nil)
	d.Limit = 2
	ctx := context.Background()
	p1, err := Run(ctx, conn, db.Resolve("libsql"), d, Request{FullRead: true, Now: time.Now()})
	if err != nil || len(p1.Rows) != 2 || p1.Rows[0]["name"] != "B" || p1.Next == nil {
		t.Fatalf("page1 %+v %v", p1, err)
	}
	mustExec("INSERT INTO `Purchase Request` VALUES ('Z','tZ','9','u','1')") // newest, sorts before cursor
	p2, err := Run(ctx, conn, db.Resolve("libsql"), d, Request{FullRead: true, Cursor: p1.Next})
	if err != nil || len(p2.Rows) != 1 || p2.Rows[0]["name"] != "C" || p2.Next != nil {
		t.Fatalf("page2 %+v %v", p2, err)
	}
}
