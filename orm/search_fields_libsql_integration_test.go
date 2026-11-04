//go:build integration

package orm

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLibSQLConfiguredSearchUsesPickerIdentifiersAndExcludesHiddenFields(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_LIBSQL_DSN to a fresh disposable loopback sqld service")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("refusing non-loopback LibSQL HTTP DSN")
	}
	conn, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP service:", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL HTTP service:", err)
	}

	table := fmt.Sprintf("tabContactSearch%d", time.Now().UnixNano()%1_000_000_000)
	quote := db.Resolve("libsql").(db.SchemaDialect).QuoteIdent
	qTable := quote(table)
	_, err = conn.Exec("CREATE TABLE " + qTable + " (name TEXT PRIMARY KEY, owner TEXT, creation TEXT, modified TEXT, modified_by TEXT, doc_status INTEGER, email TEXT, company TEXT, full_name TEXT, hidden_note TEXT, password TEXT, unindexed TEXT, unconfigured TEXT)")
	if err != nil {
		t.Fatal("create isolated picker table:", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec("DROP TABLE " + qTable) })
	for _, field := range []string{"email", "company", "full_name"} {
		if _, err := conn.Exec("CREATE INDEX " + quote(table+"_"+field+"_idx") + " ON " + qTable + " (" + quote(field) + " COLLATE NOCASE)"); err != nil {
			t.Fatal("create isolated search index:", err)
		}
	}
	if _, err := conn.Exec("INSERT INTO "+qTable+" (name, owner, creation, modified, modified_by, doc_status, email, company, full_name, hidden_note, password, unindexed, unconfigured) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", "CONTACT-0001", "owner", "2026-01-01", "2026-01-01", "owner", 0, "person@example.test", "Northwind_Research", "Ada Lovelace", "private-target", "secret-target", "unindexed-target", "internal-target"); err != nil {
		t.Fatal("insert isolated picker row:", err)
	}
	dt := &doctype.DocType{
		Name: strings.TrimPrefix(table, "tab"), TitleField: "full_name",
		SearchFields: "email, company, full_name, unindexed, hidden_note, password",
		SortField:    "name", SortOrder: "ASC",
		Fields: []doctype.Field{
			{Fieldname: "email", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "company", Fieldtype: "Link", SearchIndex: true},
			{Fieldname: "full_name", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "hidden_note", Fieldtype: "Data", Hidden: true},
			{Fieldname: "password", Fieldtype: "Password"},
			{Fieldname: "unindexed", Fieldtype: "Data"},
			{Fieldname: "unconfigured", Fieldtype: "Data"},
		},
	}
	tx := &TxManager{DB: conn, Dialect: db.Resolve("libsql")}
	docs, total, err := tx.GetListWithScope(dt, "", "", 10, 0, policy.RowScope{}, nil, "person@example.test")
	if err != nil {
		t.Fatal("search by contact email:", err)
	}
	if total != 1 || len(docs) != 1 || docs[0].Name != "CONTACT-0001" || docs[0].Get("full_name") != "Ada Lovelace" {
		t.Fatalf("email search returned total=%d docs=%#v", total, docs)
	}
	docs, total, err = tx.GetListWithScope(dt, "", "", 10, 0, policy.RowScope{}, nil, "Northwind_Research")
	if err != nil {
		t.Fatal("searching a literal underscore in an identifier:", err)
	}
	if total != 1 || len(docs) != 1 {
		t.Fatalf("literal underscore search returned total=%d docs=%#v", total, docs)
	}
	plan, err := conn.Query("EXPLAIN QUERY PLAN SELECT name FROM "+qTable+" WHERE email LIKE ? ESCAPE '!' OR company LIKE ? ESCAPE '!' OR full_name LIKE ? ESCAPE '!' OR name = ?", "person@example.test%", "person@example.test%", "person@example.test%", "person@example.test")
	if err != nil {
		t.Fatal("explain picker query:", err)
	}
	var planDetails []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			plan.Close()
			t.Fatal("scan picker query plan:", err)
		}
		planDetails = append(planDetails, detail)
	}
	if err := plan.Err(); err != nil {
		plan.Close()
		t.Fatal("read picker query plan:", err)
	}
	if err := plan.Close(); err != nil {
		t.Fatal("close picker query plan:", err)
	}
	indexed := false
	for _, detail := range planDetails {
		indexed = indexed || strings.Contains(strings.ToUpper(detail), "INDEX")
	}
	if !indexed {
		t.Errorf("picker query did not use a configured index: %v", planDetails)
	}
	for _, term := range []string{"private-target", "secret-target", "unindexed-target", "internal-target"} {
		docs, total, err = tx.GetListWithScope(dt, "", "", 10, 0, policy.RowScope{}, nil, term)
		if err != nil {
			t.Fatalf("search excluded field %q: %v", term, err)
		}
		if total != 0 || len(docs) != 0 {
			t.Errorf("search for excluded field value %q returned total=%d docs=%#v", term, total, docs)
		}
	}
}
