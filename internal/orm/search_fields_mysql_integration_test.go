//go:build integration

package orm

import (
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestMySQLConfiguredSearchUsesPrefixIndex(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_MYSQL_DSN to a fresh disposable loopback MySQL service")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse MySQL DSN:", err)
	}
	host, _, hostErr := net.SplitHostPort(parsed.Addr)
	if hostErr != nil || (host != "127.0.0.1" && host != "localhost") {
		t.Fatal("refusing non-loopback MySQL DSN")
	}
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL service:", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Fatal("ping disposable MySQL service:", err)
	}

	dialect := &db.MySQLDialect{}
	table := fmt.Sprintf("tabContactSearch%d", time.Now().UnixNano()%1_000_000_000)
	quote := dialect.QuoteIdent
	qTable := quote(table)
	_, err = conn.Exec("CREATE TABLE " + qTable + " (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL DEFAULT '', creation DATETIME NOT NULL, modified DATETIME NOT NULL, modified_by VARCHAR(140) NOT NULL DEFAULT '', doc_status SMALLINT NOT NULL DEFAULT 0, email VARCHAR(255), company VARCHAR(255), full_name VARCHAR(255), hidden_note TEXT, password VARCHAR(255), unindexed VARCHAR(255)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci")
	if err != nil {
		t.Fatal("create isolated picker table:", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec("DROP TABLE " + qTable) })
	for _, field := range []string{"email", "company", "full_name"} {
		if _, err := conn.Exec(dialect.CreateIndex(table, field, false)); err != nil {
			t.Fatal("create configured search index:", err)
		}
	}

	insert, err := conn.Prepare("INSERT INTO " + qTable + " (name, creation, modified, email, company, full_name, hidden_note, password, unindexed) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		t.Fatal("prepare disposable search rows:", err)
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5000; i++ {
		name := fmt.Sprintf("CONTACT-%05d", i)
		if _, err := insert.Exec(name, created, created, fmt.Sprintf("person-%05d@example.test", i), fmt.Sprintf("Company %05d", i), fmt.Sprintf("Customer %05d", i), "private-target", "secret-target", "unindexed-target"); err != nil {
			insert.Close()
			t.Fatal("populate disposable search rows:", err)
		}
	}
	if _, err := insert.Exec("CONTACT-TARGET", created, created, "amina.njeri@example.test", "Northwind Research", "Amina Njeri", "private-target", "secret-target", "unindexed-target"); err != nil {
		insert.Close()
		t.Fatal("insert target search row:", err)
	}
	if err := insert.Close(); err != nil {
		t.Fatal("close disposable search insert:", err)
	}
	if _, err := conn.Exec("ANALYZE TABLE " + qTable); err != nil {
		t.Fatal("analyze disposable search table:", err)
	}

	dt := &doctype.DocType{
		Name: strings.TrimPrefix(table, "tab"), TitleField: "full_name", SortField: "name", SortOrder: "ASC",
		SearchFields: "email, company, full_name, unindexed, hidden_note, password",
		Fields: []doctype.Field{
			{Fieldname: "email", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "company", Fieldtype: "Link", SearchIndex: true},
			{Fieldname: "full_name", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "hidden_note", Fieldtype: "Data", Hidden: true},
			{Fieldname: "password", Fieldtype: "Password"},
			{Fieldname: "unindexed", Fieldtype: "Data"},
		},
	}
	tx := &TxManager{DB: conn, Dialect: dialect}
	docs, total, err := tx.GetListWithScope(dt, "", "", 10, 0, policy.RowScope{}, nil, "amina")
	if err != nil {
		t.Fatal("search by visible contact name:", err)
	}
	if total != 1 || len(docs) != 1 || docs[0].Name != "CONTACT-TARGET" || docs[0].Get("full_name") != "Amina Njeri" {
		t.Fatalf("prefix search returned total=%d docs=%#v", total, docs)
	}

	plan := "EXPLAIN FORMAT=JSON SELECT name FROM " + qTable + " WHERE email LIKE ? ESCAPE '!' OR company LIKE ? ESCAPE '!' OR full_name LIKE ? ESCAPE '!' OR name = ?"
	var planJSON string
	if err := conn.QueryRow(plan, "amina%", "amina%", "amina%", "amina").Scan(&planJSON); err != nil {
		t.Fatal("explain configured prefix search:", err)
	}
	if !strings.Contains(planJSON, "idx_"+table+"_full_name") && !strings.Contains(planJSON, "idx_"+table+"_email") && !strings.Contains(planJSON, "idx_"+table+"_company") {
		t.Fatalf("MySQL prefix search did not use a configured search index: %s", planJSON)
	}

	for _, term := range []string{"private-target", "secret-target", "unindexed-target"} {
		docs, total, err = tx.GetListWithScope(dt, "", "", 10, 0, policy.RowScope{}, nil, term)
		if err != nil {
			t.Fatalf("search excluded field %q: %v", term, err)
		}
		if total != 0 || len(docs) != 0 {
			t.Errorf("search for excluded field value %q returned total=%d docs=%#v", term, total, docs)
		}
	}
}
