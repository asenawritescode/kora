package query

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func aggReg() *doctype.Registry {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "register", Fieldtype: "Data"},
		{Fieldname: "status", Fieldtype: "Data"},
		{Fieldname: "cashier", Fieldtype: "Data"},
		{Fieldname: "total", Fieldtype: "Currency"},
	}})
	return reg
}

const aggHead = `
name: totals
module_owner_id: pos
capability: pos.sales
operation: totals
source: Sale
permission: read
scope: tenant
`

func aggDef(t *testing.T, body string) *Definition {
	t.Helper()
	d, err := Parse([]byte(aggHead+body), aggReg())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func aggConn(t *testing.T, rows string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	stmts := []string{"CREATE TABLE `tabSale` (name TEXT, register TEXT, status TEXT, cashier TEXT, total NUMERIC)"}
	if rows != "" {
		stmts = append(stmts, "INSERT INTO `tabSale` VALUES "+rows)
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

const grouped = "group_by: [register]\naggregates:\n  - {name: n, fn: count}\n  - {name: total, fn: sum, field: total}\n"

func TestAggregateRun(t *testing.T) {
	rows := "('S1','R2','Paid','bob',10.5),('S2','R1','Paid','al',4),('S3','R1','Void','bob',6)"
	ctx, dl := context.Background(), db.Resolve("libsql")

	empty, err := Run(ctx, aggConn(t, ""), dl, aggDef(t, "aggregates:\n  - {name: n, fn: count}\n  - {name: total, fn: sum, field: total}\n"), Request{FullRead: true})
	if err != nil || len(empty.Rows) != 1 || empty.Rows[0]["n"] != int64(0) || empty.Rows[0]["total"] != "0" {
		t.Fatalf("empty: %v %v", empty, err)
	}
	p, err := Run(ctx, aggConn(t, rows), dl, aggDef(t, grouped), Request{FullRead: true})
	if err != nil || len(p.Rows) != 2 || p.Rows[0]["register"] != "R1" || p.Rows[0]["n"] != int64(2) || p.Rows[0]["total"] != "10" || p.Rows[1]["total"] != "10.5" {
		t.Fatalf("grouped: %v %v", p, err)
	}
	opt := grouped + "filters:\n  - {field: status, op: eq, value: $input.st, optional: true}\ninput:\n  st: {type: string}\n"
	p, err = Run(ctx, aggConn(t, rows), dl, aggDef(t, opt), Request{FullRead: true})
	if err != nil || p.Rows[0]["n"] != int64(2) {
		t.Fatalf("optional skipped: %v %v", p, err)
	}
	p, err = Run(ctx, aggConn(t, rows), dl, aggDef(t, opt), Request{FullRead: true, Input: map[string]any{"st": "Void"}})
	if err != nil || len(p.Rows) != 1 || p.Rows[0]["n"] != int64(1) {
		t.Fatalf("filtered: %v %v", p, err)
	}
	own := aggDef(t, grouped+"owner_field: cashier\n")
	p, err = Run(ctx, aggConn(t, rows), dl, own, Request{Actor: "bob"})
	if err != nil || len(p.Rows) != 2 || p.Rows[0]["n"] != int64(1) {
		t.Fatalf("owner: %v %v", p, err)
	}
	few := aggDef(t, grouped+"limit: 1\n")
	if _, err := Run(ctx, aggConn(t, rows), dl, few, Request{FullRead: true}); err == nil || !strings.Contains(err.Error(), ErrTooManyGroups) {
		t.Fatalf("too many groups: %v", err)
	}
	if _, err := Run(ctx, aggConn(t, rows), dl, aggDef(t, grouped), Request{FullRead: true, Cursor: []any{"R1"}}); err == nil || !strings.Contains(err.Error(), ErrInputInvalid) {
		t.Fatalf("cursor: %v", err)
	}
}

func TestAggregateInvalid(t *testing.T) {
	tests := map[string]string{
		"sum on Data":          "aggregates:\n  - {name: s, fn: sum, field: status}\n",
		"name collides":        "group_by: [register]\naggregates:\n  - {name: register, fn: count}\n",
		"sort on aggregate":    grouped + "sort: [{field: n}]\n",
		"fields mismatch":      grouped + "fields: [register]\n",
		"group_by no aggs":     "group_by: [register]\nfields: [register]\n",
		"count takes no field": "aggregates:\n  - {name: n, fn: count, field: total}\n",
	}
	for name, body := range tests {
		if _, err := Parse([]byte(aggHead+body), aggReg()); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	d := aggDef(t, grouped+"fields: [register, n, total]\n")
	raw, err := Encode([]*Definition{d})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decode(raw, aggReg()); err != nil || len(got[0].Fields) != 3 {
		t.Fatalf("round trip: %v %v", got, err)
	}
}
