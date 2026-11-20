package schema

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestComputeDiffRelaxesColumnsOfOptionalFields(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dialect  db.Dialect
		reqd     bool
		nullable bool
		want     string // expected DDL, "" for none
	}{
		{"mysql optional over NOT NULL", &db.MySQLDialect{}, false, false, "ALTER TABLE `tabMember` MODIFY COLUMN `code` varchar(140) NULL"},
		{"postgres optional over NOT NULL", &db.PostgresDialect{}, false, false, `ALTER TABLE "tabMember" ALTER COLUMN "code" DROP NOT NULL`},
		{"optional over nullable", &db.MySQLDialect{}, false, true, ""},
		{"required over NOT NULL", &db.MySQLDialect{}, true, false, ""},
		{"required over nullable is never tightened", &db.PostgresDialect{}, true, true, ""},
		{"libsql cannot relax in place", &db.LibSQLDialect{}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dt := &doctype.DocType{Name: "Member", Fields: []doctype.Field{{Fieldname: "code", Fieldtype: "Data", Reqd: tc.reqd, Unique: true}}}
			live := map[string]*TableInfo{"tabMember": {Name: "tabMember", Columns: map[string]*ColumnInfo{
				"code": {Name: "code", Type: "varchar(140)", Nullable: tc.nullable, Indexed: true, IndexNames: []string{"uq_tabMember_code"}, UniqueIndexNames: []string{"uq_tabMember_code"}},
			}}}
			diff := ComputeDiff([]*doctype.DocType{dt}, func(string) *doctype.DocType { return nil }, live, tc.dialect)
			ddl := diff.GenerateDDL([]*doctype.DocType{dt}, func(string) *doctype.DocType { return nil }, tc.dialect)
			if tc.want == "" {
				if len(ddl) != 0 || !diff.IsEmpty() {
					t.Fatalf("want no change, got %v", ddl)
				}
				return
			}
			if diff.IsEmpty() || len(ddl) != 1 || ddl[0] != tc.want {
				t.Fatalf("DDL = %v, want [%s]", ddl, tc.want)
			}
		})
	}
}

func TestMySQLDropNotNullKeepsDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field doctype.Field
		live  string
		want  string
	}{
		{"live type kept", doctype.Field{Fieldname: "code", Fieldtype: "Data"}, "varchar(60)", "`code` varchar(60) NULL"},
		{"field type when live type unknown", doctype.Field{Fieldname: "code", Fieldtype: "Data"}, "", "`code` VARCHAR(140) NULL"},
		{"default restated", doctype.Field{Fieldname: "state", Fieldtype: "Select", Default: "Open"}, "varchar(140)", "`state` varchar(140) NULL DEFAULT 'Open'"},
		{"no default on TEXT", doctype.Field{Fieldname: "notes", Fieldtype: "Text", Default: "x"}, "text", "`notes` text NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&db.MySQLDialect{}).DropNotNull("tabMember", &tc.field, tc.live)
			if !strings.HasSuffix(got, "MODIFY COLUMN "+tc.want) {
				t.Fatalf("DDL = %q, want suffix %q", got, tc.want)
			}
		})
	}
}

func TestRequirednessChangeTiers(t *testing.T) {
	oldDT := &doctype.DocType{Name: "Member"}
	for _, tc := range []struct {
		name           string
		oldReqd, reqd  bool
		def, tier, ddl string
		changed        bool
	}{
		{"optional is safe and relaxes", true, false, "", "safe", "ALTER TABLE `tabMember` MODIFY COLUMN `code` VARCHAR(140) NULL", true},
		{"required without default warns", false, true, "", "warning", "", true},
		{"required with default is safe", false, true, "X", "safe", "", true},
		{"unchanged", true, true, "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldF := &doctype.Field{Fieldname: "code", Fieldtype: "Data", Reqd: tc.oldReqd}
			newF := &doctype.Field{Fieldname: "code", Fieldtype: "Data", Reqd: tc.reqd, Default: tc.def}
			change, changed := requirednessChange(oldDT, oldF, newF, 3, &db.MySQLDialect{})
			if changed != tc.changed || change.Tier != tc.tier || change.DDL != tc.ddl {
				t.Fatalf("change = %+v (changed %v)", change, changed)
			}
		})
	}
}
