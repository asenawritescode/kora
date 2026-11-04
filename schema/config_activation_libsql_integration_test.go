//go:build integration

package schema

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestApplySnapshotActivationDDLUsesLibSQLHTTPBatch(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_LIBSQL_DSN to a fresh disposable loopback sqld service")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("refusing non-loopback LibSQL HTTP DSN")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP service:", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL HTTP service:", err)
	}
	dialect := db.Resolve("libsql")
	table := fmt.Sprintf("kora_activation_plan_%d", time.Now().UnixNano()%1_000_000_000)
	index := table + "_idx"
	statements := []string{
		"CREATE TABLE " + table + " (id INTEGER PRIMARY KEY, name TEXT NOT NULL)",
		"CREATE INDEX " + index + " ON " + table + " (name)",
	}
	if err := ApplySnapshotActivationDDL(context.Background(), database, dialect, statements); err != nil {
		t.Fatal("apply LibSQL DDL over HTTP batch:", err)
	}
	if err := ApplySnapshotActivationDDL(context.Background(), database, dialect, statements); err != nil {
		t.Fatalf("repeat partially-applied LibSQL plan should recover idempotently: %v", err)
	}
	var found int
	if err := database.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&found); err != nil {
		t.Fatal("verify applied DDL:", err)
	}
	if found != 1 {
		t.Fatalf("created table count = %d, want 1", found)
	}
	if _, err := database.Exec("DROP TABLE " + table); err != nil {
		t.Fatal("remove isolated test table:", err)
	}
}

func TestLibSQLCompanyNameUniqueRelaxationIsRetryable(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_LIBSQL_DSN to a fresh disposable loopback sqld service")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("refusing non-loopback LibSQL HTTP DSN")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP service:", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL HTTP service:", err)
	}

	table := fmt.Sprintf("tabCRMCompany_%d", time.Now().UnixNano()%1_000_000_000)
	uniqueIndex := "uq_" + table + "_company_name"
	dialect := db.Resolve("libsql")
	searchIndex := dialect.(db.SearchIndexDialect).SearchIndexName(table, "company_name")
	quote := dialect.(db.SchemaDialect).QuoteIdent
	if _, err := database.Exec("CREATE TABLE " + quote(table) + " (company_name TEXT NOT NULL)"); err != nil {
		t.Fatal("create isolated company table:", err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec("DROP TABLE " + quote(table))
	})
	if _, err := database.Exec("CREATE UNIQUE INDEX " + quote(uniqueIndex) + " ON " + quote(table) + " (company_name)"); err != nil {
		t.Fatal("create legacy unique index:", err)
	}

	dt := &doctype.DocType{Name: strings.TrimPrefix(table, "tab"), Fields: []doctype.Field{{Fieldname: "company_name", Fieldtype: "Data", SearchIndex: true}}}
	indexes, err := database.Query("PRAGMA index_list('" + table + "')")
	if err != nil {
		t.Fatal("read initial indexes:", err)
	}
	var indexNames []string
	for indexes.Next() {
		var seq, unique int
		var name, origin string
		var partial int
		if err := indexes.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			indexes.Close()
			t.Fatal("scan initial index:", err)
		}
		indexNames = append(indexNames, name)
	}
	if err := indexes.Err(); err != nil {
		indexes.Close()
		t.Fatal("iterate initial indexes:", err)
	}
	if err := indexes.Close(); err != nil {
		t.Fatal("close initial indexes:", err)
	}

	live := map[string]*TableInfo{table: {Name: table, Columns: map[string]*ColumnInfo{
		"company_name": {Name: "company_name", Indexed: true, IndexNames: indexNames, UniqueIndexNames: []string{uniqueIndex}},
	}}}
	diff := ComputeDiff([]*doctype.DocType{dt}, func(string) *doctype.DocType { return nil }, live, dialect)
	statements := diff.GenerateDDL([]*doctype.DocType{dt}, func(string) *doctype.DocType { return nil }, dialect)
	if len(statements) != 2 || !strings.Contains(statements[0], searchIndex) || !strings.Contains(statements[0], "COLLATE NOCASE") || !strings.Contains(statements[1], uniqueIndex) {
		t.Fatalf("migration statements = %#v, want case-insensitive search index before unique-index drop", statements)
	}
	for i, statement := range statements {
		if err := ApplySnapshotActivationDDL(context.Background(), database, dialect, []string{statement}); err != nil {
			t.Fatalf("apply migration statement %d: %v", i, err)
		}
	}
	// The activation journal may resume by replaying a partially applied DDL
	// plan. Both CREATE INDEX IF NOT EXISTS and absence-aware planning must make
	// that replay harmless.
	if err := ApplySnapshotActivationDDL(context.Background(), database, dialect, statements); err != nil {
		t.Fatalf("replay completed migration plan: %v", err)
	}
	if _, err := database.Exec("INSERT INTO "+quote(table)+" (company_name) VALUES (?), (?)", "Northwind Ltd", "Northwind Ltd"); err != nil {
		t.Fatalf("duplicate company name should be accepted after upgrade: %v", err)
	}
	var found int
	if err := database.QueryRow("SELECT COUNT(*) FROM pragma_index_list(?) WHERE name = ? AND "+quote("unique")+" = 0", table, searchIndex).Scan(&found); err != nil {
		t.Fatal("verify replacement non-unique index:", err)
	}
	if found != 1 {
		t.Fatalf("replacement search index count = %d, want 1", found)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM pragma_index_list(?) WHERE name = ?", table, uniqueIndex).Scan(&found); err != nil {
		t.Fatal("verify old unique index removed:", err)
	}
	if found != 0 {
		t.Fatalf("old unique index count = %d, want 0", found)
	}
}

func TestLibSQLCRMReadableReferenceUniqueRelaxationIsRetryable(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_LIBSQL_DSN to a fresh disposable loopback sqld service")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("refusing non-loopback LibSQL HTTP DSN")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP service:", err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL HTTP service:", err)
	}

	dialect := db.Resolve("libsql")
	schemaDialect := dialect.(db.SchemaDialect)
	searchDialect := dialect.(db.SearchIndexDialect)
	for _, doctypeName := range []string{"Qualification", "Risk", "Handoff"} {
		doctypeName := doctypeName
		t.Run(doctypeName, func(t *testing.T) {
			table := fmt.Sprintf("tabCRM%s_%d", strings.ReplaceAll(doctypeName, " ", ""), time.Now().UnixNano()%1_000_000_000)
			column := "reference"
			uniqueIndex := "uq_" + table + "_" + column
			quote := schemaDialect.QuoteIdent
			if _, err := database.Exec("CREATE TABLE " + quote(table) + " (reference TEXT NOT NULL)"); err != nil {
				t.Fatal("create isolated legacy table:", err)
			}
			t.Cleanup(func() { _, _ = database.Exec("DROP TABLE " + quote(table)) })
			if _, err := database.Exec("CREATE UNIQUE INDEX " + quote(uniqueIndex) + " ON " + quote(table) + " (reference)"); err != nil {
				t.Fatal("create legacy unique index:", err)
			}
			indexes, err := database.Query("PRAGMA index_list('" + table + "')")
			if err != nil {
				t.Fatal("read legacy indexes:", err)
			}
			var indexNames []string
			for indexes.Next() {
				var seq, unique, partial int
				var name, origin string
				if err := indexes.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
					indexes.Close()
					t.Fatal("scan legacy index:", err)
				}
				indexNames = append(indexNames, name)
			}
			if err := indexes.Err(); err != nil {
				indexes.Close()
				t.Fatal("iterate legacy indexes:", err)
			}
			if err := indexes.Close(); err != nil {
				t.Fatal("close legacy indexes:", err)
			}

			dt := &doctype.DocType{Name: strings.TrimPrefix(table, "tab"), Fields: []doctype.Field{{Fieldname: column, Fieldtype: "Data", SearchIndex: true}}}
			live := map[string]*TableInfo{table: {Name: table, Columns: map[string]*ColumnInfo{
				column: {Name: column, Indexed: true, IndexNames: indexNames, UniqueIndexNames: []string{uniqueIndex}},
			}}}
			findDocType := func(string) *doctype.DocType { return nil }
			statements := ComputeDiff([]*doctype.DocType{dt}, findDocType, live, dialect).GenerateDDL([]*doctype.DocType{dt}, findDocType, dialect)
			searchIndex := searchDialect.SearchIndexName(table, column)
			if len(statements) != 2 || !strings.Contains(statements[0], searchIndex) || !strings.Contains(statements[1], uniqueIndex) {
				t.Fatalf("migration statements = %#v; want search index created before legacy unique index is dropped", statements)
			}
			for i, statement := range statements {
				if err := ApplySnapshotActivationDDL(context.Background(), database, dialect, []string{statement}); err != nil {
					t.Fatalf("apply migration statement %d: %v", i, err)
				}
			}
			if err := ApplySnapshotActivationDDL(context.Background(), database, dialect, statements); err != nil {
				t.Fatal("replay completed migration plan:", err)
			}
			if _, err := database.Exec("INSERT INTO "+quote(table)+" (reference) VALUES (?), (?)", "Discovery review", "Discovery review"); err != nil {
				t.Fatalf("duplicate readable titles should be accepted after upgrade: %v", err)
			}
			var found int
			if err := database.QueryRow("SELECT COUNT(*) FROM pragma_index_list(?) WHERE name = ?", table, searchIndex).Scan(&found); err != nil || found != 1 {
				t.Fatalf("search index count = %d, error %v; want 1", found, err)
			}
			if err := database.QueryRow("SELECT COUNT(*) FROM pragma_index_list(?) WHERE name = ?", table, uniqueIndex).Scan(&found); err != nil || found != 0 {
				t.Fatalf("legacy unique index count = %d, error %v; want 0", found, err)
			}
		})
	}
}
