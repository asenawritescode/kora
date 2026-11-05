package query

import (
	"context"
	"database/sql"

	"github.com/asenawritescode/kora/db"
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestPOSPackQueries(t *testing.T) {
	dts, err := doctype.ParseDirectory("../config/pos/doctypes")
	if err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	for _, d := range dts {
		reg.Register(d)
	}
	defs, err := LoadDir("../config/pos/queries", reg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"open_till": "Till Session", "recent_sales": "Sale", "today_totals": "Sale"}
	if len(defs) != len(want) {
		t.Fatalf("got %d queries, want %d", len(defs), len(want))
	}
	for _, d := range defs {
		if want[d.Name] != d.Source {
			t.Errorf("%s: source %q, want %q", d.Name, d.Source, want[d.Name])
		}
	}
}

func TestPOSTodayTotals(t *testing.T) {
	dts, err := doctype.ParseDirectory("../config/pos/doctypes")
	if err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	for _, d := range dts {
		reg.Register(d)
	}
	defs, err := LoadDir("../config/pos/queries", reg)
	if err != nil {
		t.Fatal(err)
	}
	var def *Definition
	for _, d := range defs {
		if d.Name == "today_totals" {
			def = d
		}
	}
	if def == nil {
		t.Fatal("today_totals missing")
	}
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	for _, s := range []string{
		"CREATE TABLE `tabSale` (name TEXT, register TEXT, status TEXT, creation TEXT, total NUMERIC)",
		"INSERT INTO `tabSale` VALUES ('S1','R1','Paid','2026-01-02 10:00:00',10),('S2','R1','Paid','2026-01-02 11:00:00',5),('S3','R1','Draft','2026-01-02 12:00:00',99),('S4','R1','Paid','2025-12-31 10:00:00',77),('S5','R2','Paid','2026-01-02 09:00:00',7)",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	dl := db.Resolve("libsql")
	p, err := Run(context.Background(), conn, dl, def, Request{FullRead: true, Input: map[string]any{"since": "2026-01-01 00:00:00"}})
	if err != nil || len(p.Rows) != 2 {
		t.Fatalf("all: %v %v", p, err)
	}
	if r := p.Rows[0]; r["register"] != "R1" || r["sale_count"] != int64(2) || r["total"] != "15" {
		t.Fatalf("R1: %v", r)
	}
	if r := p.Rows[1]; r["register"] != "R2" || r["sale_count"] != int64(1) || r["total"] != "7" {
		t.Fatalf("R2: %v", r)
	}
	p, err = Run(context.Background(), conn, dl, def, Request{FullRead: true, Input: map[string]any{"since": "2026-01-01 00:00:00", "register": "R2"}})
	if err != nil || len(p.Rows) != 1 {
		t.Fatalf("single: %v %v", p, err)
	}
}
