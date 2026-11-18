package schema

import (
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/doctype"
)

// A Date field defaulting to "today" is filled by the ORM, so ADD COLUMN must
// carry no DEFAULT. A required one has no database default and is a Warning,
// the same tier as a required field with no default.
func TestAnalyzeImpact_OrmFilledDefaultAddColumn(t *testing.T) {
	tests := []struct {
		name     string
		field    doctype.Field
		wantTier string
		wantDB   bool // whether the ADD COLUMN may carry a DEFAULT
	}{
		{"required date today is warning", doctype.Field{Fieldname: "visited_on", Fieldtype: "Date", Default: "today", Reqd: true}, "warning", false},
		{"optional date today is safe", doctype.Field{Fieldname: "visited_on", Fieldtype: "Date", Default: "today"}, "safe", false},
		{"required literal date is safe", doctype.Field{Fieldname: "visited_on", Fieldtype: "Date", Default: "2024-01-01", Reqd: true}, "safe", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer database.Close()
			mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabVisit`").
				WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(3))

			oldDT := &doctype.DocType{Name: "Visit", Fields: []doctype.Field{{Fieldname: "code", Fieldtype: "Data"}}}
			newDT := &doctype.DocType{Name: "Visit", Fields: []doctype.Field{{Fieldname: "code", Fieldtype: "Data"}, tt.field}}
			reg := doctype.NewRegistry()
			reg.Register(newDT)

			preview := AnalyzeImpact(database, oldDT, newDT, reg, newMySQLDialect())
			var ddl string
			for _, c := range preview.Changes {
				if c.Field == "visited_on" {
					if c.Tier != tt.wantTier {
						t.Errorf("tier = %q, want %q", c.Tier, tt.wantTier)
					}
					ddl = c.DDL
				}
			}
			if ddl == "" {
				t.Fatal("no DDL for visited_on")
			}
			if got := strings.Contains(ddl, "DEFAULT"); got != tt.wantDB {
				t.Errorf("ADD COLUMN DEFAULT present = %v, want %v: %s", got, tt.wantDB, ddl)
			}
			if strings.Contains(strings.ToUpper(ddl), "CURRENT_DATE") || strings.Contains(strings.ToUpper(ddl), "'TODAY'") {
				t.Errorf("ADD COLUMN carries today default: %s", ddl)
			}
		})
	}
}
