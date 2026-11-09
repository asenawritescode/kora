package query

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestInventoryPackQueries(t *testing.T) {
	pack := filepath.Join("..", "..", "config", "inventory")
	dts, err := doctype.ParseDirectory(filepath.Join(pack, "doctypes"))
	if err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	for _, dt := range dts {
		reg.Register(dt)
	}
	defs, err := LoadDir(filepath.Join(pack, "queries"), reg)
	if err != nil || len(defs) != 4 {
		t.Fatalf("load %d %v", len(defs), err)
	}
	by := map[string]*Definition{}
	for _, d := range defs {
		by[d.Name] = d
	}
	conn, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Skip(err)
	}
	conn.SetMaxOpenConns(1)
	for _, s := range []string{
		"CREATE TABLE `tabItem` (name TEXT, item_name TEXT, barcode TEXT, uom TEXT, price NUMERIC, category TEXT, image TEXT)",
		"CREATE TABLE `tabItem Category` (name TEXT, category_name TEXT, display_order INTEGER, enabled INTEGER)",
		"CREATE TABLE `tabStock Balance` (name TEXT, item TEXT, location TEXT, quantity REAL, reorder_level REAL)",
		"INSERT INTO `tabItem` VALUES ('I1','Blue Pen','111','Each',1.5,'C2','/files/blue.png'),('I2','Red Pen','222','Each',2,'C2',NULL),('I3','Notebook','333','Each',5,'C1',NULL)",
		"INSERT INTO `tabItem Category` VALUES ('C1','Paper',2,1),('C2','Pens',1,1),('C3','Retired',0,0),('C4','Art',2,1)",
		"INSERT INTO `tabStock Balance` VALUES ('B1','I1','L1',7,5),('B2','I1','L2',3,5),('B3','I2','L1',4,4),('B4','I3','L1',0,0),('B5','I3','L2',9,10)",
	} {
		if _, err := conn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		query string
		in    map[string]any
		want  int
		first string
	}{
		{"item_search", map[string]any{"term": "pen"}, 2, "I1"},
		{"item_search", map[string]any{"term": "333"}, 1, "I3"},
		{"item_search", nil, 3, "I1"},
		{"item_search", map[string]any{"term": "111"}, 1, "I1"},
		{"item_search", map[string]any{"category": "C2"}, 2, "I1"},
		{"item_search", map[string]any{"category": "C1"}, 1, "I3"},
		{"item_search", map[string]any{"category": "C2", "term": "red"}, 1, "I2"},
		{"item_search", map[string]any{"category": "none"}, 0, ""},
		{"item_categories", nil, 3, "C2"},
		{"item_stock", map[string]any{"location": "L1"}, 3, "B1"},
		{"item_stock", map[string]any{"item": "I1"}, 2, "B1"},
		{"item_stock", map[string]any{"item": "I1", "location": "L2"}, 1, "B2"},
		{"low_stock", nil, 3, "B2"},
		{"low_stock", map[string]any{"location": "L1"}, 1, "B3"},
	}
	for _, tc := range tests {
		p, err := Run(context.Background(), conn, db.Resolve("libsql"), by[tc.query], Request{Input: tc.in, FullRead: true})
		if err != nil || len(p.Rows) != tc.want || tc.want > 0 && p.Rows[0]["name"] != tc.first {
			t.Errorf("%s %v: rows=%v err=%v", tc.query, tc.in, p, err)
		}
	}
	// Tile fields reach the client and the categories come back in display order.
	p, err := Run(context.Background(), conn, db.Resolve("libsql"), by["item_search"], Request{Input: map[string]any{"term": "111"}, FullRead: true})
	if err != nil || p.Rows[0]["category"] != "C2" || p.Rows[0]["image"] != "/files/blue.png" {
		t.Errorf("item_search tile fields: rows=%v err=%v", p, err)
	}
	p, err = Run(context.Background(), conn, db.Resolve("libsql"), by["item_categories"], Request{FullRead: true})
	var got []any
	for _, r := range p.Rows {
		got = append(got, r["name"])
	}
	if err != nil || !reflect.DeepEqual(got, []any{"C2", "C4", "C1"}) || p.Rows[0]["category_name"] != "Pens" {
		t.Errorf("item_categories order = %v err=%v", got, err)
	}
	for _, f := range []string{"category", "image"} {
		if !slices.Contains(by["item_search"].OutputFields(), f) {
			t.Errorf("item_search allowed fields missing %s", f)
		}
	}
}
