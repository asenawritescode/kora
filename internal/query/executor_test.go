package query

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
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
		{"optional skipped", db.MySQL(), []Filter{opt}, Request{Actor: "u", FullRead: true}, []string{"FROM `tabPurchase Request` ORDER BY `modified` DESC, `name` ASC LIMIT 3"}, 0, false},
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
	mustExec("CREATE TABLE `tabPurchase Request` (name TEXT, title TEXT, modified TEXT, owner TEXT, total NUMERIC)")
	for _, r := range [][2]string{{"A", "2"}, {"B", "3"}, {"C", "1"}} {
		mustExec("INSERT INTO `tabPurchase Request` VALUES (?,?,?,?,?)", r[0], "t"+r[0], r[1], "u", "1.50")
	}
	d := def(nil)
	d.Limit = 2
	ctx := context.Background()
	p1, err := Run(ctx, conn, db.Resolve("libsql"), d, Request{FullRead: true, Now: time.Now()})
	if err != nil || len(p1.Rows) != 2 || p1.Rows[0]["name"] != "B" || p1.Next == nil {
		t.Fatalf("page1 %+v %v", p1, err)
	}
	mustExec("INSERT INTO `tabPurchase Request` VALUES ('Z','tZ','9','u','1')") // newest, sorts before cursor
	p2, err := Run(ctx, conn, db.Resolve("libsql"), d, Request{FullRead: true, Cursor: p1.Next})
	if err != nil || len(p2.Rows) != 1 || p2.Rows[0]["name"] != "C" || p2.Next != nil {
		t.Fatalf("page2 %+v %v", p2, err)
	}
}

func TestFieldRefFilter(t *testing.T) {
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec("CREATE TABLE `tabPurchase Request` (name TEXT, title TEXT, modified TEXT, owner TEXT, qty INT, lvl INT)"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		n    string
		q, l int
	}{{"A", 1, 5}, {"B", 5, 5}, {"C", 9, 5}} {
		if _, err := conn.Exec("INSERT INTO `tabPurchase Request` VALUES (?,?,?,?,?,?)", r.n, "t", r.n, "u", r.q, r.l); err != nil {
			t.Fatal(err)
		}
	}
	fl := []Filter{
		{Field: "qty", Op: "lte", Value: "$field.lvl"},
		{Any: []Filter{{Field: "name", Op: "ne", Value: "$input.search"}}},
	}
	d := def(fl)
	d.Limit = 10
	req := Request{FullRead: true, Input: map[string]any{"search": "B"}}
	q, args, _, err := Build(d, db.Resolve("libsql"), req)
	if err != nil || !strings.Contains(q, `"qty" <= "lvl"`) || len(args) != 1 || args[0] != "B" {
		t.Fatalf("q=%s args=%v err=%v", q, args, err)
	}
	p, err := Run(context.Background(), conn, db.Resolve("libsql"), d, req)
	if err != nil || len(p.Rows) != 1 || p.Rows[0]["name"] != "A" {
		t.Fatalf("rows %+v %v", p.Rows, err)
	}
	req.Input = nil
	d.Filters = fl[:1]
	if p, err = Run(context.Background(), conn, db.Resolve("libsql"), d, req); err != nil || len(p.Rows) != 2 {
		t.Fatalf("rows %+v %v", p.Rows, err)
	}
}

func TestInputDefaultsAndRequired(t *testing.T) {
	now := time.Date(2026, 11, 6, 15, 4, 5, 0, time.UTC)
	since := Filter{Field: "creation", Op: "gte", Value: "$input.since"}
	tests := []struct {
		name      string
		spec      InputSpec
		input     map[string]any
		wantArg   any
		wantField string // non-empty: expect *InputError naming it
	}{
		{"missing and no default is a caller error", InputSpec{Type: "string"}, nil, nil, "since"},
		{"null counts as missing", InputSpec{Type: "string"}, map[string]any{"since": nil}, nil, "since"},
		{"default $today", InputSpec{Type: "string", Default: "$today"}, nil, "2026-11-06", ""},
		{"default $now", InputSpec{Type: "string", Default: "$now"}, nil, "2026-11-06 15:04:05", ""},
		{"literal default", InputSpec{Type: "string", Default: "2020-01-01"}, nil, "2020-01-01", ""},
		{"supplied input wins over default", InputSpec{Type: "string", Default: "$today"}, map[string]any{"since": "2026-01-01"}, "2026-01-01", ""},
		{"published default $today sent back resolves like omitted", InputSpec{Type: "string", Default: "$today"}, map[string]any{"since": "$today"}, "2026-11-06", ""},
		{"published default $now sent back resolves like omitted", InputSpec{Type: "string", Default: "$now"}, map[string]any{"since": "$now"}, "2026-11-06 15:04:05", ""},
		{"published default $actor sent back resolves like omitted", InputSpec{Type: "string", Default: "$actor"}, map[string]any{"since": "$actor"}, "alice", ""},
		{"reference token other than the declared default stays literal", InputSpec{Type: "string", Default: "$today"}, map[string]any{"since": "$now"}, "$now", ""},
		{"token with no declared default stays literal", InputSpec{Type: "string"}, map[string]any{"since": "$today"}, "$today", ""},
		{"literal default is not special", InputSpec{Type: "string", Default: "2020-01-01"}, map[string]any{"since": "2020-01-01"}, "2020-01-01", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := def([]Filter{since})
			d.Input = map[string]InputSpec{"since": tc.spec}
			_, args, _, err := Build(d, db.MySQL(), Request{FullRead: true, Now: now, Actor: "alice", Input: tc.input})
			if tc.wantField != "" {
				var ie *InputError
				if !errors.As(err, &ie) || ie.Field != tc.wantField || !strings.Contains(ie.Error(), ErrInputInvalid) {
					t.Fatalf("err = %v, want *InputError for %s", err, tc.wantField)
				}
				return
			}
			if err != nil || len(args) != 1 || args[0] != tc.wantArg {
				t.Fatalf("args=%v err=%v, want %v", args, err, tc.wantArg)
			}
		})
	}
}

func TestRequiredInputs(t *testing.T) {
	opt := func(in string) Filter { return Filter{Field: "a", Op: "eq", Value: "$input." + in, Optional: true} }
	req := func(in string) Filter { return Filter{Field: "a", Op: "eq", Value: "$input." + in} }
	tests := []struct {
		name    string
		filters []Filter
		input   map[string]InputSpec
		want    []string
	}{
		{"none", []Filter{opt("a")}, map[string]InputSpec{"a": {Type: "string"}}, nil},
		{"required sorted", []Filter{req("b"), req("a"), opt("c")}, map[string]InputSpec{"a": {}, "b": {}, "c": {}}, []string{"a", "b"}},
		{"defaulted is not required", []Filter{req("a")}, map[string]InputSpec{"a": {Type: "string", Default: "$today"}}, nil},
		{"in any group", []Filter{{Any: []Filter{req("a"), opt("b")}}}, map[string]InputSpec{"a": {}, "b": {}}, []string{"a"}},
		{"literals ignored", []Filter{{Field: "a", Op: "eq", Value: "Paid"}}, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := (&Definition{Filters: tc.filters, Input: tc.input}).RequiredInputs()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
