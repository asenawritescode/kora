package query

import (
	"context"
	"database/sql"
	"time"

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
	want := map[string]string{"open_till": "Till Session", "my_session": "Till Session", "recent_sales": "Sale", "today_totals": "Sale", "payments_by_method": "Sale Payment", "refunds_by_method": "Sale Payment"}
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

// TestPOSPackQueriesAcceptEmptyInput is the #317 regression: Studio's Owner
// dashboard calls every published query with no input and none may fail.
func TestPOSPackQueriesAcceptEmptyInput(t *testing.T) {
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
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, s := range []string{
		"CREATE TABLE `tabSale` (name TEXT, register TEXT, customer TEXT, status TEXT, till_session TEXT, creation TEXT, total NUMERIC)",
		"CREATE TABLE `tabSale__payments` (name TEXT, parent TEXT, method TEXT, amount NUMERIC)",
		"CREATE TABLE `tabTill Session` (name TEXT, shift TEXT, register TEXT, location TEXT, cashier TEXT, status TEXT, opening_float NUMERIC, expected_cash NUMERIC, closing_count NUMERIC, creation TEXT)",
		"INSERT INTO `tabSale` VALUES ('S1','R1','C1','Paid','TS1','2026-11-06 09:00:00',10),('S2','R1','C1','Paid','TS1','2026-11-05 09:00:00',99),('S3','R1','C1','Return','TS1','2026-11-06 10:00:00',4)",
		"INSERT INTO `tabSale__payments` VALUES ('P1','S1','Cash',10),('P2','S2','Cash',99),('P3','S3','Cash',4)",
		"INSERT INTO `tabTill Session` VALUES ('TS1','SH1','R1','L1','c1','Open',5,15,NULL,'2026-11-06 08:00:00'),('TS0','SH0','R1','L1','c1','Closed',5,5,5,'2026-11-05 08:00:00')",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	// my_session has no actor here, so it matches nobody.
	wantRows := map[string]int{"my_session": 0, "open_till": 1, "recent_sales": 3, "today_totals": 1, "payments_by_method": 1, "refunds_by_method": 1}
	for _, d := range defs {
		t.Run(d.Name, func(t *testing.T) {
			p, err := Run(context.Background(), conn, db.Resolve("libsql"), d, Request{FullRead: true, Now: now, Input: map[string]any{}})
			if err != nil {
				t.Fatalf("empty input must run: %v", err)
			}
			if len(p.Rows) != wantRows[d.Name] {
				t.Fatalf("rows = %v, want %d", p.Rows, wantRows[d.Name])
			}
		})
	}
	// today_totals defaults to today: S2 (yesterday) is excluded.
	for _, d := range defs {
		if d.Name != "today_totals" {
			continue
		}
		p, err := Run(context.Background(), conn, db.Resolve("libsql"), d, Request{FullRead: true, Now: now, Input: map[string]any{}})
		if err != nil || len(p.Rows) != 1 || p.Rows[0]["total"] != "10" || p.Rows[0]["sale_count"] != int64(1) {
			t.Fatalf("today_totals default: %v %v", p, err)
		}
	}
}

// TestPOSMySession (#326): my_session returns the actor's open till with the
// register and location a sale needs, and nothing for anyone else.
func TestPOSMySession(t *testing.T) {
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
		if d.Name == "my_session" {
			def = d
		}
	}
	if def == nil {
		t.Fatal("my_session missing")
	}
	if def.OwnerField != "cashier" || def.Limit != 1 {
		t.Fatalf("my_session owner_field=%q limit=%d", def.OwnerField, def.Limit)
	}
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	for _, s := range []string{
		"CREATE TABLE `tabTill Session` (name TEXT, shift TEXT, register TEXT, location TEXT, cashier TEXT, status TEXT, opening_float NUMERIC, expected_cash NUMERIC, creation TEXT)",
		"INSERT INTO `tabTill Session` VALUES ('TS1','SH1','R1','L1','ann','Open',500,500,'2026-11-06 08:00:00'),('TS0','SH0','R1','L1','ann','Closed',5,5,'2026-11-05 08:00:00'),('TS2','SH2','R2','L1','bob','Open',100,100,'2026-11-06 09:00:00')",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, full := range []bool{true, false} {
		p, err := Run(context.Background(), conn, db.Resolve("libsql"), def, Request{FullRead: full, Actor: "ann", Input: map[string]any{}})
		if err != nil || len(p.Rows) != 1 {
			t.Fatalf("full=%v ann: %v %v", full, p, err)
		}
		if r := p.Rows[0]; r["name"] != "TS1" || r["register"] != "R1" || r["location"] != "L1" || r["shift"] != "SH1" {
			t.Fatalf("ann row = %v", r)
		}
		p, err = Run(context.Background(), conn, db.Resolve("libsql"), def, Request{FullRead: full, Actor: "carol", Input: map[string]any{}})
		if err != nil || len(p.Rows) != 0 {
			t.Fatalf("full=%v carol sees %v %v", full, p, err)
		}
	}
}
