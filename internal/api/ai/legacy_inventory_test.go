package ai

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
)

func TestInspectRunStatusInventory(t *testing.T) {
	for _, test := range []struct {
		name    string
		dialect string
	}{
		{name: "mysql", dialect: "mysql"},
		{name: "postgres", dialect: "postgres"},
		{name: "libsql", dialect: "libsql"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			dialect := kdb.Resolve(test.dialect)
			if test.dialect == "libsql" {
				mock.ExpectQuery("PRAGMA table_info\\('_kora_ai_run'\\)").WillReturnRows(sqlmock.NewRows([]string{"cid", "name", "type", "notnull", "dflt_value", "pk"}).
					AddRow(0, "id", "TEXT", 1, nil, 1).
					AddRow(1, "site", "TEXT", 1, "''", 0).
					AddRow(2, "status", "TEXT", 1, "'planning'", 0))
			} else {
				query := `SELECT column_name FROM information_schema.columns WHERE table_schema=`
				if test.dialect == "mysql" {
					query += `DATABASE() AND table_name=?`
				} else {
					query += `current_schema() AND table_name=$1`
				}
				mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("_kora_ai_run").WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id").AddRow("site").AddRow("status"))
			}
			statusQuery := `SELECT status,COUNT(*) FROM _kora_ai_run WHERE site=` + dialect.Placeholder(1) + ` GROUP BY status ORDER BY status LIMIT 501`
			mock.ExpectQuery(regexp.QuoteMeta(statusQuery)).WithArgs("tenant-1").WillReturnRows(sqlmock.NewRows([]string{"status", "count"}).AddRow("completed", 3).AddRow("planning", 1))

			got, err := InspectRunStatusInventory(context.Background(), database, dialect, "tenant-1")
			if err != nil {
				t.Fatalf("InspectRunStatusInventory() error = %v", err)
			}
			if !got.TablePresent || len(got.Statuses) != 2 || got.Statuses[0] != (RunStatusCount{Status: "completed", Rows: 3}) || got.Statuses[1] != (RunStatusCount{Status: "planning", Rows: 1}) {
				t.Fatalf("InspectRunStatusInventory() = %#v", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet SQL expectations: %v", err)
			}
		})
	}
}

func TestInspectRunStatusInventoryDoesNotCreateMissingTable(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`)).
		WithArgs("_kora_ai_run").WillReturnRows(sqlmock.NewRows([]string{"column_name"}))

	got, err := InspectRunStatusInventory(context.Background(), database, kdb.Resolve("mysql"), "tenant-1")
	if err != nil || got.TablePresent || len(got.Statuses) != 0 {
		t.Fatalf("missing-table inventory = %#v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInspectRunStatusInventoryFailsClosedWithoutExposingDatabaseErrors(t *testing.T) {
	t.Run("unknown schema", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`)).
			WithArgs("_kora_ai_run").WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id").AddRow("status"))

		got, err := InspectRunStatusInventory(context.Background(), database, kdb.Resolve("mysql"), "tenant-1")
		if err == nil || !got.TablePresent || errors.Is(err, sqlmock.ErrCancelled) {
			t.Fatalf("unknown schema inventory = %#v, %v", got, err)
		}
		if err.Error() != "unrecognized schema for _kora_ai_run" {
			t.Fatalf("unexpected schema error contains implementation detail: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("query failure", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT column_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`)).
			WithArgs("_kora_ai_run").WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id").AddRow("site").AddRow("status"))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT status,COUNT(*) FROM _kora_ai_run WHERE site=? GROUP BY status ORDER BY status LIMIT 501`)).
			WithArgs("tenant-1").WillReturnError(errors.New("sensitive connection string"))

		_, err = InspectRunStatusInventory(context.Background(), database, kdb.Resolve("mysql"), "tenant-1")
		if err == nil || err.Error() != "aggregate AI run status counts" {
			t.Fatalf("query failure exposed a driver error: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}
