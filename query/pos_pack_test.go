package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	want := map[string]string{"open_till": "Till Session", "my_session": "Till Session", "my_registers": "Register", "recent_sales": "Sale", "sale_lines": "Sale Line Balance", "today_totals": "Sale", "payments_by_method": "Sale Payment", "refunds_by_method": "Sale Payment"}
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
		"CREATE TABLE `tabRegister` (name TEXT, register_name TEXT, location TEXT, enabled INTEGER)",
		"INSERT INTO `tabRegister` VALUES ('R1','Front','L1',1),('R0','Old','L1',0)",
		"INSERT INTO `tabSale` VALUES ('S1','R1','C1','Paid','TS1','2026-11-06 09:00:00',10),('S2','R1','C1','Paid','TS1','2026-11-05 09:00:00',99),('S3','R1','C1','Return','TS1','2026-11-06 10:00:00',4)",
		"INSERT INTO `tabSale__payments` VALUES ('P1','S1','Cash',10),('P2','S2','Cash',99),('P3','S3','Cash',4)",
		"INSERT INTO `tabTill Session` VALUES ('TS1','SH1','R1','L1','c1','Open',5,15,NULL,'2026-11-06 08:00:00'),('TS0','SH0','R1','L1','c1','Closed',5,5,5,'2026-11-05 08:00:00')",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	// my_session has no actor here, so it matches nobody.
	wantRows := map[string]int{"my_session": 0, "my_registers": 1, "open_till": 1, "recent_sales": 3, "today_totals": 1, "payments_by_method": 1, "refunds_by_method": 1}
	for _, d := range defs {
		if d.Name == "sale_lines" {
			continue // its sale input is required: TestPOSSaleLines pins the 400
		}
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
		// #334: Studio sends the published default back; it must behave like an omitted input.
		echoed := map[string]any{"since": d.Input["since"].Default}
		if echoed["since"] != "$today" {
			t.Fatalf("published default = %v, want $today", echoed["since"])
		}
		if p, err = Run(context.Background(), conn, db.Resolve("libsql"), d, Request{FullRead: true, Now: now, Input: echoed}); err != nil || len(p.Rows) != 1 || p.Rows[0]["total"] != "10" {
			t.Fatalf("today_totals with echoed default: %v %v", p, err)
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

// TestPOSMyRegisters (#374): my_registers lists the enabled registers with the
// location a till is opened at, ordered by name, for any actor.
func TestPOSMyRegisters(t *testing.T) {
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
		if d.Name == "my_registers" {
			def = d
		}
	}
	if def == nil {
		t.Fatal("my_registers missing")
	}
	if def.OwnerField != "" || def.Limit < 1 || def.Limit > 100 {
		t.Fatalf("my_registers owner_field=%q limit=%d", def.OwnerField, def.Limit)
	}
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	for _, s := range []string{
		"CREATE TABLE `tabRegister` (name TEXT, register_name TEXT, location TEXT, enabled INTEGER, owner TEXT, creation TEXT)",
		"INSERT INTO `tabRegister` VALUES ('REG-2','Side','LOC-1',1,'x','2026-11-06 08:00:00'),('REG-1','Front','LOC-1',1,'x','2026-11-06 09:00:00'),('REG-3','Old','LOC-2',0,'x','2026-11-06 07:00:00'),('REG-4','Back','LOC-2',1,'y','2026-11-06 10:00:00')",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, full := range []bool{true, false} {
		for _, actor := range []string{"ann", "bob"} {
			p, err := Run(context.Background(), conn, db.Resolve("libsql"), def, Request{FullRead: full, Actor: actor, Input: map[string]any{}})
			if err != nil || len(p.Rows) != 3 {
				t.Fatalf("full=%v %s: %v %v", full, actor, p, err)
			}
			for i, want := range [][3]string{{"REG-4", "Back", "LOC-2"}, {"REG-1", "Front", "LOC-1"}, {"REG-2", "Side", "LOC-1"}} {
				if r := p.Rows[i]; r["name"] != want[0] || r["register_name"] != want[1] || r["location"] != want[2] || len(r) != 3 {
					t.Fatalf("full=%v %s row %d = %v, want %v", full, actor, i, r, want)
				}
			}
		}
	}
}

// TestPOSSaleLines (#329): sale_lines lists one sale's returnable lines from
// the Sale Line Balance rows, with what is left to return, by item.
func TestPOSSaleLines(t *testing.T) {
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
		if d.Name == "sale_lines" {
			def = d
		}
	}
	if def == nil {
		t.Fatal("sale_lines missing")
	}
	if def.OwnerField != "" || def.Limit < 1 || def.Limit > 200 {
		t.Fatalf("sale_lines owner_field=%q limit=%d", def.OwnerField, def.Limit)
	}
	if got := def.RequiredInputs(); len(got) != 1 || got[0] != "sale" {
		t.Fatalf("required inputs = %v, want [sale]", got)
	}
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	for _, s := range []string{
		"CREATE TABLE `tabSale Line Balance` (name TEXT, sale TEXT, item TEXT, unit_price NUMERIC, sold_quantity NUMERIC, remaining_quantity NUMERIC, owner TEXT, creation TEXT)",
		// SALE-1: ITEM-2 partly returned, ITEM-1 untouched, ITEM-3 fully returned.
		"INSERT INTO `tabSale Line Balance` VALUES ('B3','SALE-1','ITEM-2','65.50',1.5,0.5,'ann','2026-11-06 09:00:00'),('B1','SALE-1','ITEM-3','10.25',2,0,'ann','2026-11-06 09:00:00'),('B2','SALE-1','ITEM-1','180.00',2,2,'ann','2026-11-06 09:00:00'),('B4','SALE-2','ITEM-1','180.00',1,1,'bob','2026-11-06 10:00:00')",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, full := range []bool{true, false} {
		p, err := Run(context.Background(), conn, db.Resolve("libsql"), def, Request{FullRead: full, Actor: "carol", Input: map[string]any{"sale": "SALE-1"}})
		if err != nil || len(p.Rows) != 3 {
			t.Fatalf("full=%v: %v %v", full, p, err)
		}
		for i, want := range [][5]string{{"B2", "ITEM-1", "180", "2", "2"}, {"B3", "ITEM-2", "65.5", "1.5", "0.5"}, {"B1", "ITEM-3", "10.25", "2", "0"}} {
			r := p.Rows[i]
			got := [5]string{fmt.Sprint(r["name"]), fmt.Sprint(r["item"]), fmt.Sprint(r["unit_price"]), fmt.Sprint(r["sold_quantity"]), fmt.Sprint(r["remaining_quantity"])}
			if got != want || len(r) != 5 {
				t.Fatalf("full=%v row %d = %#v, want %v", full, i, r, want)
			}
		}
	}
	p, err := Run(context.Background(), conn, db.Resolve("libsql"), def, Request{FullRead: true, Input: map[string]any{"sale": "SALE-404"}})
	if err != nil || len(p.Rows) != 0 {
		t.Fatalf("unknown sale: %v %v", p, err)
	}
	_, err = Run(context.Background(), conn, db.Resolve("libsql"), def, Request{FullRead: true, Input: map[string]any{}})
	var ie *InputError
	if !errors.As(err, &ie) || ie.Field != "sale" {
		t.Fatalf("missing sale err = %v, want InputError on sale", err)
	}
}
