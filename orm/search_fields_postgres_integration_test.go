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
	_ "github.com/lib/pq"
)

func TestPostgresConfiguredSearchUsesPrefixIndex(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_POSTGRES_DSN to a fresh disposable loopback PostgreSQL service")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("refusing non-loopback PostgreSQL DSN")
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL service:", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL service:", err)
	}

	dtName := fmt.Sprintf("ContactSearch%d", time.Now().UnixNano()%1_000_000_000)
	dt := &doctype.DocType{
		Name: dtName, TitleField: "full_name", SearchFields: "email, full_name",
		SortField: "name", SortOrder: "ASC",
		Fields: []doctype.Field{
			{Fieldname: "email", Fieldtype: "Data", SearchIndex: true},
			{Fieldname: "full_name", Fieldtype: "Data", SearchIndex: true},
		},
	}
	dialect := db.Resolve("postgres")
	q := dialect.(db.SchemaDialect).QuoteIdent
	table := dt.RawTableName()
	qTable := q(table)
	if _, err := conn.Exec("CREATE TABLE " + qTable + " (name TEXT PRIMARY KEY, email TEXT, full_name TEXT, owner TEXT, creation TIMESTAMP, modified TIMESTAMP, modified_by TEXT, doc_status INTEGER)"); err != nil {
		t.Fatal("create isolated search table:", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec("DROP TABLE " + qTable) })
	searchDialect := dialect.(db.SearchIndexDialect)
	for _, field := range []string{"email", "full_name"} {
		if _, err := conn.Exec(searchDialect.CreateSearchIndex(table, field)); err != nil {
			t.Fatal("create PostgreSQL prefix index:", err)
		}
	}
	if _, err := conn.Exec("INSERT INTO " + qTable + " (name, email, full_name) SELECT 'CONTACT-' || n, 'ordinary' || n || '@example.test', 'Ordinary Person ' || n FROM generate_series(1, 10000) AS n"); err != nil {
		t.Fatal("populate disposable PostgreSQL search table:", err)
	}
	if _, err := conn.Exec("INSERT INTO " + qTable + " (name, email, full_name) VALUES ('CONTACT-TARGET', 'needle@example.test', 'Needle Person')"); err != nil {
		t.Fatal("insert target PostgreSQL search row:", err)
	}
	if _, err := conn.Exec("ANALYZE " + qTable); err != nil {
		t.Fatal("analyze disposable search table:", err)
	}
	if _, err := conn.Exec("SET enable_seqscan = off"); err != nil {
		t.Fatal("force index-path verification:", err)
	}
	plan, err := conn.Query("EXPLAIN SELECT name FROM "+qTable+" WHERE email LIKE $1 ESCAPE '!' OR full_name LIKE $2 ESCAPE '!' OR name = $3", "needle%", "needle%", "needle")
	if err != nil {
		t.Fatal("explain configured search:", err)
	}
	var details []string
	for plan.Next() {
		var detail string
		if err := plan.Scan(&detail); err != nil {
			plan.Close()
			t.Fatal("scan PostgreSQL query plan:", err)
		}
		details = append(details, detail)
	}
	if err := plan.Err(); err != nil {
		plan.Close()
		t.Fatal("read PostgreSQL query plan:", err)
	}
	if err := plan.Close(); err != nil {
		t.Fatal("close PostgreSQL query plan:", err)
	}
	joinedPlan := strings.Join(details, "\n")
	if !strings.Contains(joinedPlan, searchDialect.SearchIndexName(table, "email")) && !strings.Contains(joinedPlan, searchDialect.SearchIndexName(table, "full_name")) {
		t.Fatalf("PostgreSQL prefix search did not use a search index:\n%s", joinedPlan)
	}
	tx := &TxManager{DB: conn, Dialect: dialect}
	docs, total, err := tx.GetListWithScope(dt, "", "", 10, 0, policy.RowScope{}, nil, "needle@example.test")
	if err != nil {
		t.Fatal("execute configured PostgreSQL search:", err)
	}
	if total != 1 || len(docs) != 1 || docs[0].Name != "CONTACT-TARGET" || docs[0].Get("full_name") != "Needle Person" {
		t.Fatalf("PostgreSQL picker search returned total=%d docs=%#v", total, docs)
	}
}
