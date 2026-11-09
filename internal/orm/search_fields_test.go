package orm

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

func TestGetListSearchUsesConfiguredVisibleFields(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	dt := &doctype.DocType{
		Name:         "Contact",
		TitleField:   "full_name",
		SearchFields: "email, company, full_name, unindexed, hidden_note, password, unknown, email",
		SortField:    "name",
		SortOrder:    "ASC",
		Fields: []doctype.Field{
			{Fieldname: "email", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "full_name", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "company", Fieldtype: "Link", SearchIndex: true},
			{Fieldname: "hidden_note", Fieldtype: "Data", Hidden: true},
			{Fieldname: "password", Fieldtype: "Password"},
			{Fieldname: "unindexed", Fieldtype: "Data"},
			{Fieldname: "unconfigured", Fieldtype: "Data"},
		},
	}

	countSQL := regexp.QuoteMeta("SELECT COUNT(*) FROM `tabContact` WHERE 1=1 AND (`email` LIKE ? ESCAPE '!' OR `company` LIKE ? ESCAPE '!' OR `full_name` LIKE ? ESCAPE '!' OR `name` = ?)")
	mock.ExpectQuery(countSQL).
		WithArgs("person!%!_!!@example.test%", "person!%!_!!@example.test%", "person!%!_!!@example.test%", "person%_!@example.test").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	selectSQL := regexp.QuoteMeta("SELECT `email`, `full_name`, `company`, `hidden_note`, `password`, `unindexed`, `unconfigured`, `name`, `owner`, `creation`, `modified`, `modified_by`, `doc_status` FROM `tabContact` WHERE 1=1 AND (`email` LIKE ? ESCAPE '!' OR `company` LIKE ? ESCAPE '!' OR `full_name` LIKE ? ESCAPE '!' OR `name` = ?) ORDER BY `name` ASC LIMIT ? OFFSET ?")
	mock.ExpectQuery(selectSQL).
		WithArgs("person!%!_!!@example.test%", "person!%!_!!@example.test%", "person!%!_!!@example.test%", "person%_!@example.test", 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"email", "full_name", "company", "hidden_note", "password", "unindexed", "unconfigured", "name", "owner", "creation", "modified", "modified_by", "doc_status"}))

	tx := &TxManager{DB: conn, Dialect: db.Resolve("mysql")}
	if _, _, err := tx.GetListWithScope(dt, "", "", 10, 0, policy.RowScope{}, nil, "person%_!@example.test"); err != nil {
		t.Fatalf("GetListWithScope: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetListSearchIncludesIndexedLinkedTitle(t *testing.T) {
	conn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name: "Company", TitleField: "company_name",
		Fields: []doctype.Field{{Fieldname: "company_name", Fieldtype: "Data", SearchIndex: true}},
	})
	deal := &doctype.DocType{
		Name: "Deal", TitleField: "deal_name", SearchFields: "deal_name, company",
		SortField: "name", SortOrder: "ASC",
		Fields: []doctype.Field{
			{Fieldname: "deal_name", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "company", Fieldtype: "Link", Options: "Company", SearchIndex: true},
		},
	}
	countSQL := regexp.QuoteMeta("SELECT COUNT(*) FROM `tabDeal` WHERE 1=1 AND (`deal_name` LIKE ? ESCAPE '!' OR `company` LIKE ? ESCAPE '!' OR `company` IN (SELECT `name` FROM `tabCompany` WHERE `company_name` LIKE ? ESCAPE '!') OR `name` = ?)")
	mock.ExpectQuery(countSQL).WithArgs("Acacia%", "Acacia%", "Acacia%", "Acacia").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	selectSQL := regexp.QuoteMeta("SELECT `deal_name`, `company`, `name`, `owner`, `creation`, `modified`, `modified_by`, `doc_status` FROM `tabDeal` WHERE 1=1 AND (`deal_name` LIKE ? ESCAPE '!' OR `company` LIKE ? ESCAPE '!' OR `company` IN (SELECT `name` FROM `tabCompany` WHERE `company_name` LIKE ? ESCAPE '!') OR `name` = ?) ORDER BY `name` ASC LIMIT ? OFFSET ?")
	mock.ExpectQuery(selectSQL).WithArgs("Acacia%", "Acacia%", "Acacia%", "Acacia", 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"deal_name", "company", "name", "owner", "creation", "modified", "modified_by", "doc_status"}))
	tx := &TxManager{DB: conn, Registry: reg, Dialect: db.Resolve("mysql")}
	if _, _, err := tx.GetListWithScope(deal, "", "", 10, 0, policy.RowScope{}, nil, "Acacia"); err != nil {
		t.Fatalf("GetListWithScope: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredSearchFieldsDeduplicatesAndTrims(t *testing.T) {
	dt := &doctype.DocType{SearchFields: " email, company,email, , phone "}
	got := configuredSearchFieldNames(dt)
	want := []string{"email", "company", "phone"}
	if len(got) != len(want) {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fields = %#v, want %#v", got, want)
		}
	}
}
