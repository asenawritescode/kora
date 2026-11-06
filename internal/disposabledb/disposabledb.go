// Package disposabledb gives integration tests a database of their own.
//
// The integration suites share one MySQL and one PostgreSQL server (CI service
// containers or a local disposable one). A test that mutates schema or counts
// rows must not share a database with the tests that ran before it, so it asks
// for a fresh database derived from the DSN the harness provides.
package disposabledb

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"testing"

	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// Database is one disposable database: the open handle, its name and a DSN that
// reaches it.
type Database struct {
	DB   *sql.DB
	Name string
	DSN  string
}

// Open creates a uniquely named database on the server behind dsn, opens it and
// drops it when the test ends. The DSN may or may not name a database: for MySQL
// the name is ignored, for PostgreSQL it is the maintenance database the new one
// is created from.
func Open(t testing.TB, driver, dsn string) *Database {
	t.Helper()
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("name disposable database: %v", err)
	}
	name := "kora_t_" + hex.EncodeToString(random[:])
	var adminDSN, target, create, drop string
	switch driver {
	case "mysql":
		config, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("parse MySQL DSN: %v", err)
		}
		config.DBName = ""
		adminDSN = config.FormatDSN()
		config.DBName = name
		target = config.FormatDSN()
		create = fmt.Sprintf("CREATE DATABASE `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", name)
		drop = fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", name)
	case "postgres":
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse PostgreSQL DSN: %v", err)
		}
		adminDSN = dsn
		parsed.Path = "/" + name
		target = parsed.String()
		create = fmt.Sprintf(`CREATE DATABASE "%s"`, name)
		drop = fmt.Sprintf(`DROP DATABASE IF EXISTS "%s" WITH (FORCE)`, name)
	default:
		t.Fatalf("disposabledb: unsupported driver %q", driver)
	}
	admin, err := sql.Open(driver, adminDSN)
	if err != nil {
		t.Fatalf("open %s server: %v", driver, err)
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Fatalf("ping %s server: %v", driver, err)
	}
	if _, err := admin.Exec(create); err != nil {
		_ = admin.Close()
		t.Fatalf("create disposable %s database: %v", driver, err)
	}
	database, err := sql.Open(driver, target)
	if err != nil {
		_, _ = admin.Exec(drop)
		_ = admin.Close()
		t.Fatalf("open disposable %s database: %v", driver, err)
	}
	t.Cleanup(func() {
		_ = database.Close()
		_, _ = admin.Exec(drop)
		_ = admin.Close()
	})
	return &Database{DB: database, Name: name, DSN: target}
}
