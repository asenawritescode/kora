//go:build integration

package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	ksite "github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
)

const (
	benchRecordCount = 1000
	benchChildRows   = 3
	benchPageSize    = 20
	benchOrder       = "Perf Order"
	benchLine        = "Perf Line"
	benchCustomer    = "Perf Customer"
	benchUser        = "bench@example.test"
)

// benchStatements counts every server round trip the wrapped drivers issue:
// prepares, executes, queries, transaction begin/commit/rollback.
var benchStatements = &kernelSQLMetrics{}

func init() {
	sql.Register("count-mysql", &benchCountingDriver{inner: &mysql.MySQLDriver{}})
	sql.Register("count-postgres", &benchCountingDriver{inner: &pq.Driver{}})
}

// benchCountingDriver is test-only. It wraps a real driver and counts
// statements without changing what reaches the server.
type benchCountingDriver struct{ inner driver.Driver }

func (d *benchCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &benchCountingConn{Conn: conn}, nil
}

type benchCountingConn struct{ driver.Conn }

func (c *benchCountingConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *benchCountingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	benchStatements.recordStatement()
	var stmt driver.Stmt
	var err error
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		stmt, err = preparer.PrepareContext(ctx, query)
	} else {
		stmt, err = c.Conn.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &benchCountingStmt{Stmt: stmt}, nil
}

func (c *benchCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, query, args)
	if !errors.Is(err, driver.ErrSkip) {
		benchStatements.recordStatement()
	}
	return result, err
}

func (c *benchCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if !errors.Is(err, driver.ErrSkip) {
		benchStatements.recordStatement()
	}
	return rows, err
}

// BeginTx counts the BEGIN. Drivers without ConnBeginTx fall back to the
// uncounted Begin; both drivers wired here implement ConnBeginTx.
func (c *benchCountingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, driver.ErrSkip
	}
	tx, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	benchStatements.recordStatement()
	return &benchCountingTx{Tx: tx}, nil
}

// benchCountingStmt counts context-aware executions. The legacy Exec and Query
// are promoted from the wrapped statement; both drivers wired here implement
// the context variants, so database/sql never reaches them.
type benchCountingStmt struct{ driver.Stmt }

func (s *benchCountingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	result, err := execer.ExecContext(ctx, args)
	if !errors.Is(err, driver.ErrSkip) {
		benchStatements.recordStatement()
	}
	return result, err
}

func (s *benchCountingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, args)
	if !errors.Is(err, driver.ErrSkip) {
		benchStatements.recordStatement()
	}
	return rows, err
}

type benchCountingTx struct{ driver.Tx }

func (tx *benchCountingTx) Commit() error {
	benchStatements.recordStatement()
	return tx.Tx.Commit()
}

func (tx *benchCountingTx) Rollback() error {
	benchStatements.recordStatement()
	return tx.Tx.Rollback()
}

// benchSite is a real site: system tables, migrated DocTypes, the production
// resource routes on a gin engine, and a signed-in admin session.
type benchSite struct {
	router *gin.Engine
	db     *sql.DB
	names  []string // seeded Perf Order names
}

// benchSiteCache holds one seeded site per dialect for the whole test process.
// Seeding 1,000 records through the route costs far more than the measured
// operations, and the framework calls each benchmark once per b.N pass, so the
// site is built once and TestMain drops its database or schema afterwards.
type benchSiteCache struct {
	mu       sync.Mutex
	sites    map[string]*benchSite
	cleanups []func()
}

var benchCache = &benchSiteCache{sites: map[string]*benchSite{}}

func TestMain(m *testing.M) {
	code := m.Run()
	benchCache.close()
	os.Exit(code)
}

func (c *benchSiteCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
	c.cleanups = nil
	c.sites = map[string]*benchSite{}
}

// site returns the seeded site for dialect, building it on first use.
func (c *benchSiteCache) site(tb testing.TB, dialect string) *benchSite {
	tb.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sites[dialect]; ok {
		return s
	}
	s := newBenchSite(tb, dialect, &c.cleanups)
	c.sites[dialect] = s
	return s
}

// benchEachDialect runs fn on MySQL and Postgres. Each dialect skips unless its
// DSN variable is set, the same variables the other API integration tests use.
func benchEachDialect(b *testing.B, fn func(b *testing.B, s *benchSite)) {
	b.Helper()
	for _, dialect := range []struct{ name, dsnEnv string }{
		{"mysql", "KORA_TEST_DSN"},
		{"postgres", "KORA_API_LIVE_POSTGRES_DSN"},
	} {
		b.Run(dialect.name, func(b *testing.B) {
			if os.Getenv(dialect.dsnEnv) == "" {
				b.Skipf("set %s to a disposable %s database", dialect.dsnEnv, dialect.name)
			}
			fn(b, benchCache.site(b, dialect.name))
		})
	}
}

func newBenchSite(tb testing.TB, dialect string, cleanups *[]func()) *benchSite {
	tb.Helper()
	database, site := openBenchDatabase(tb, dialect, cleanups)
	dialectImpl := kdb.Resolve(dialect)
	if err := ksite.BootstrapSystemTables(context.Background(), database, dialectImpl); err != nil {
		tb.Fatal("bootstrap system tables:", err)
	}
	reg := benchRegistry()
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dialectImpl); err != nil {
		tb.Fatal("migrate bench DocTypes:", err)
	}
	writer := outbox.NewSQLWriter(dialectImpl)
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialectImpl, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}

	gin.SetMode(gin.TestMode)
	execution := integrationTestExecution(tb, site, benchUser, []string{doctype.AdminRole})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", site)
		c.Set("site_db", database)
		c.Set("site_registry", reg)
		c.Set("site_db_type", dialect)
		c.Set("user", benchUser)
		c.Set("user_role", doctype.AdminRole)
		c.Set("user_roles", []string{doctype.AdminRole})
		c.Set("policy_execution", execution)
		c.Next()
	})
	resource := router.Group("/api/resource")
	resource.GET("/:doctype", handler.HandleList)
	resource.POST("/:doctype", handler.HandleCreate)
	resource.GET("/:doctype/:name", handler.HandleGet)
	resource.PUT("/:doctype/:name", handler.HandleUpdate)
	resource.DELETE("/:doctype/:name", handler.HandleDelete)

	s := &benchSite{router: router, db: database}
	customer := s.createName(tb, benchPath(benchCustomer, ""), `{"customer_name":"Acme"}`)
	for i := 0; i < benchRecordCount; i++ {
		s.names = append(s.names, s.createName(tb, benchPath(benchOrder, ""), benchOrderBody(i, customer)))
	}
	return s
}

// openBenchDatabase creates an isolated MySQL database or Postgres schema and
// returns a counting handle onto it. Its drop is appended to cleanups.
func openBenchDatabase(tb testing.TB, dialect string, cleanups *[]func()) (*sql.DB, string) {
	tb.Helper()
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	var admin, database *sql.DB
	var err error
	switch dialect {
	case "mysql":
		dsn := os.Getenv("KORA_TEST_DSN")
		name := "kora_rp0_" + suffix
		admin, err = sql.Open("mysql", dsn)
		if err != nil {
			tb.Fatal("open MySQL:", err)
		}
		if _, err = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			tb.Fatal("create isolated MySQL database:", err)
		}
		*cleanups = append(*cleanups, func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = admin.Close() })
		cfg, parseErr := mysql.ParseDSN(dsn)
		if parseErr != nil {
			tb.Fatal("parse MySQL DSN:", parseErr)
		}
		cfg.DBName = name
		cfg.Params = map[string]string{"parseTime": "true", "charset": "utf8mb4"}
		database, err = sql.Open("count-mysql", cfg.FormatDSN())
		if err != nil {
			tb.Fatal("open counting MySQL:", err)
		}
		return limitBenchPool(tb, database, cleanups), name
	case "postgres":
		dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
		schemaName := "kora_rp0_" + suffix
		admin, err = sql.Open("postgres", dsn)
		if err != nil {
			tb.Fatal("open PostgreSQL:", err)
		}
		if _, err = admin.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
			tb.Fatal("create isolated PostgreSQL schema:", err)
		}
		*cleanups = append(*cleanups, func() { _, _ = admin.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`); _ = admin.Close() })
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			tb.Fatal("parse PostgreSQL DSN:", parseErr)
		}
		query := parsed.Query()
		query.Set("search_path", schemaName)
		parsed.RawQuery = query.Encode()
		database, err = sql.Open("count-postgres", parsed.String())
		if err != nil {
			tb.Fatal("open counting PostgreSQL:", err)
		}
		return limitBenchPool(tb, database, cleanups), schemaName
	}
	tb.Fatalf("unknown dialect %q", dialect)
	return nil, ""
}

func limitBenchPool(tb testing.TB, database *sql.DB, cleanups *[]func()) *sql.DB {
	tb.Helper()
	database.SetMaxOpenConns(4)
	*cleanups = append(*cleanups, func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		tb.Fatal("ping bench database:", err)
	}
	return database
}

func benchRegistry() *doctype.Registry {
	customer := &doctype.DocType{Name: benchCustomer, SortField: "modified", SortOrder: "DESC", Fields: []doctype.Field{
		{Fieldname: "customer_name", Fieldtype: "Data", Reqd: true},
	}}
	line := &doctype.DocType{Name: benchLine, IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "sku", Fieldtype: "Data", Reqd: true},
		{Fieldname: "qty", Fieldtype: "Int"},
		{Fieldname: "amount", Fieldtype: "Currency"},
	}}
	order := &doctype.DocType{Name: benchOrder, SortField: "modified", SortOrder: "DESC", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data", Reqd: true},
		{Fieldname: "serial", Fieldtype: "Data", Unique: true},
		{Fieldname: "customer", Fieldtype: "Link", Options: benchCustomer},
		{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nOpen\nClosed"},
		{Fieldname: "order_date", Fieldtype: "Date"},
		{Fieldname: "ship_date", Fieldtype: "Date"},
		{Fieldname: "quantity", Fieldtype: "Int"},
		{Fieldname: "unit_price", Fieldtype: "Currency"},
		{Fieldname: "total", Fieldtype: "Currency"},
		{Fieldname: "note", Fieldtype: "Data"},
		{Fieldname: "lines", Fieldtype: "Table", Options: benchLine},
	}}
	reg := doctype.NewRegistry()
	permissions := []*doctype.Permission{}
	for _, name := range []string{benchCustomer, benchOrder} {
		permissions = append(permissions, &doctype.Permission{
			Doctype: name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true,
		})
	}
	reg.LoadFull([]*doctype.DocType{customer, line, order}, []*doctype.Role{{Name: doctype.AdminRole}}, permissions)
	return reg
}

func benchOrderBody(i int, customer string) string {
	lines := make([]string, 0, benchChildRows)
	for j := 0; j < benchChildRows; j++ {
		lines = append(lines, fmt.Sprintf(`{"sku":"SKU-%d-%d","qty":%d,"amount":%d.50}`, i, j, j+1, (j+1)*100))
	}
	return fmt.Sprintf(`{"title":"Order %d","serial":"SO-%06d","customer":%q,"status":"Open",`+
		`"order_date":"2026-11-01","ship_date":"2026-11-08","quantity":%d,"unit_price":12.25,`+
		`"total":%d.00,"note":"bench","lines":[%s]}`,
		i, i, customer, i%50+1, 100+i, strings.Join(lines, ","))
}

func benchPath(doctypeName, name string) string {
	path := "/api/resource/" + url.PathEscape(doctypeName)
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path
}

func (s *benchSite) do(method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	s.router.ServeHTTP(recorder, request)
	return recorder
}

// createName creates a record through the route and returns its generated name.
func (s *benchSite) createName(tb testing.TB, path, body string) string {
	tb.Helper()
	recorder := s.do(http.MethodPost, path, body)
	if recorder.Code != http.StatusCreated {
		tb.Fatalf("POST %s returned HTTP %d: %s", path, recorder.Code, recorder.Body.String())
	}
	data := requireRESTDocument(tb, recorder.Body.Bytes(), doctypeFromPath(path))
	name, ok := data["name"].(string)
	if !ok || name == "" {
		tb.Fatalf("POST %s response has no record name", path)
	}
	return name
}

func doctypeFromPath(path string) string {
	segment := strings.Split(strings.TrimPrefix(path, "/api/resource/"), "/")[0]
	unescaped, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return unescaped
}

// startMeasure resets the statement counter and timer after untimed setup.
func startMeasure(b *testing.B) {
	b.Helper()
	benchStatements.reset()
	b.ReportAllocs()
	b.ResetTimer()
}

// reportStatements publishes round trips per operation next to ns/op and allocs/op.
func reportStatements(b *testing.B) {
	b.Helper()
	statements, _ := benchStatements.snapshot()
	b.ReportMetric(float64(statements)/float64(b.N), "stmts/op")
}

// BenchmarkListRecords measures one page of 20 Perf Order rows.
func BenchmarkListRecords(b *testing.B) {
	benchEachDialect(b, func(b *testing.B, s *benchSite) {
		path := benchPath(benchOrder, "") + fmt.Sprintf("?limit=%d", benchPageSize)
		startMeasure(b)
		for i := 0; i < b.N; i++ {
			if recorder := s.do(http.MethodGet, path, ""); recorder.Code != http.StatusOK {
				b.Fatalf("list returned HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
		}
		reportStatements(b)
	})
}

// BenchmarkGetRecord reads a Perf Order that has benchChildRows child rows.
func BenchmarkGetRecord(b *testing.B) {
	benchEachDialect(b, func(b *testing.B, s *benchSite) {
		startMeasure(b)
		for i := 0; i < b.N; i++ {
			path := benchPath(benchOrder, s.names[i%len(s.names)])
			if recorder := s.do(http.MethodGet, path, ""); recorder.Code != http.StatusOK {
				b.Fatalf("get returned HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
		}
		reportStatements(b)
	})
}

// benchCreateSeq keeps created serials unique across b.N passes of one process.
var benchCreateSeq atomic.Int64

// BenchmarkCreateRecord creates a new Perf Order with three child rows.
func BenchmarkCreateRecord(b *testing.B) {
	benchEachDialect(b, func(b *testing.B, s *benchSite) {
		customer := s.names[0]
		startMeasure(b)
		for i := 0; i < b.N; i++ {
			body := benchOrderBody(benchRecordCount+int(benchCreateSeq.Add(1)), customer)
			if recorder := s.do(http.MethodPost, benchPath(benchOrder, ""), body); recorder.Code != http.StatusCreated {
				b.Fatalf("create returned HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
		}
		reportStatements(b)
	})
}

// BenchmarkUpdateRecord updates the parent fields of a seeded Perf Order.
func BenchmarkUpdateRecord(b *testing.B) {
	benchEachDialect(b, func(b *testing.B, s *benchSite) {
		startMeasure(b)
		for i := 0; i < b.N; i++ {
			path := benchPath(benchOrder, s.names[i%len(s.names)])
			body := fmt.Sprintf(`{"note":"updated-%d"}`, i)
			if recorder := s.do(http.MethodPut, path, body); recorder.Code != http.StatusOK {
				b.Fatalf("update returned HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
		}
		reportStatements(b)
	})
}
