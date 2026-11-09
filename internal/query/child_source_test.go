package query

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func childReg() *doctype.Registry {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "payments", Fieldtype: "Table", Options: "Sale Payment"},
		{Fieldname: "notes", Fieldtype: "Table", Options: "Other"},
		{Fieldname: "pw", Fieldtype: "Password"},
	}})
	reg.Register(&doctype.DocType{Name: "Sale Payment", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "method", Fieldtype: "Data"},
		{Fieldname: "amount", Fieldtype: "Currency"},
	}})
	return reg
}

const childAgg = `
name: z
module_owner_id: m
capability: c
operation: o
source: Sale Payment
via: Sale.payments
permission: read
scope: tenant
filters:
  - {field: parent.status, op: eq, value: Paid}
group_by: [method]
aggregates: [{name: n, fn: count}, {name: total, fn: sum, field: amount}]
`

func TestParseChildSource(t *testing.T) {
	tests := []struct{ name, yaml, wantErr string }{
		{"valid", childAgg, ""},
		{"no via", strings.Replace(childAgg, "via: Sale.payments\n", "", 1), "needs via"},
		{"via on non-child", strings.Replace(childAgg, "source: Sale Payment", "source: Sale", 1), "only allowed on a child"},
		{"via not table", strings.Replace(childAgg, "Sale.payments", "Sale.status", 1), "not a Table field"},
		{"via options mismatch", strings.Replace(childAgg, "Sale.payments", "Sale.notes", 1), "not a Table field"},
		{"unknown parent field", strings.Replace(childAgg, "parent.status", "parent.nope", 1), "not on Sale"},
		{"parent in group_by", strings.Replace(childAgg, "group_by: [method]", "group_by: [parent.status]", 1), "group_by"},
		{"parent in sort", childAgg + "sort: [{field: parent.status}]\n", "sort"},
		{"parent in list fields", strings.Replace(childAgg, "group_by: [method]\naggregates: [{name: n, fn: count}, {name: total, fn: sum, field: amount}]", "fields: [parent.status]", 1), "fields"},
		{"owner_field", childAgg + "owner_field: owner\n", "owner_field"},
		{"parent column ok", strings.Replace(childAgg, "group_by: [method]", "group_by: [method, parent]", 1) + "fields: [method, parent, n, total]\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Parse([]byte(tt.yaml), childReg())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := Encode([]*Definition{d})
				back, err := Decode(raw, childReg())
				if err != nil || back[0].Via != "Sale.payments" {
					t.Fatalf("round trip: %v %+v", err, back)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildChildSourceSQL(t *testing.T) {
	d := db.Resolve("mysql")
	join := "FROM `tabSale__payments` `c` JOIN `tabSale` `p` ON `c`.`parent` = `p`.`name`"
	agg, _ := Parse([]byte(childAgg), childReg())
	q, args, _, err := Build(agg, d, Request{ScopeField: "entity", ScopeValue: "E1"})
	want := "SELECT `c`.`method`, COUNT(*), COALESCE(SUM(`c`.`amount`), 0) " + join + " WHERE `p`.`status` = ? AND `p`.`entity` = ? GROUP BY `c`.`method` ORDER BY `c`.`method` ASC LIMIT 51"
	if err != nil || q != want || len(args) != 2 {
		t.Fatalf("agg:\n%s\n%v %v", q, args, err)
	}
	list, _ := Parse([]byte(strings.Replace(childAgg, "group_by: [method]\naggregates: [{name: n, fn: count}, {name: total, fn: sum, field: amount}]", "fields: [method, amount]\nsort: [{field: method}]", 1)), childReg())
	q, _, _, err = Build(list, db.Resolve("postgres"), Request{Cursor: []any{"Cash", "P1"}})
	if err != nil || !strings.Contains(q, `FROM "tabSale__payments" "c" JOIN "tabSale" "p" ON "c"."parent" = "p"."name"`) ||
		!strings.Contains(q, `("c"."method" > $2) OR ("c"."method" = $3 AND "c"."name" > $4)`) || !strings.Contains(q, `ORDER BY "c"."method" ASC, "c"."name" ASC`) {
		t.Fatalf("list: %s %v", q, err)
	}
}
