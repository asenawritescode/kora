package script

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
)

func TestLoadActiveScriptsTxUsesSourceTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	store := &Store{DB: database, Dialect: kdb.Resolve("mysql")}

	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin source transaction:", err)
	}
	query := `SELECT name, site, script_type, doctype, event, method_path, schedule,
		priority, is_active, run_as, timeout_ms, script, compiled_at, compile_error,
		created_by, updated_by, creation, modified
		FROM _kora_script
		WHERE site = ? AND is_active = ? AND script_type = ? AND doctype = ?
		ORDER BY priority ASC`
	created := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(query)).
		WithArgs("tenant-a", true, string(TypeDocEvent), "Invoice").
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "site", "script_type", "doctype", "event", "method_path", "schedule",
			"priority", "is_active", "run_as", "timeout_ms", "script", "compiled_at", "compile_error",
			"created_by", "updated_by", "creation", "modified",
		}).AddRow("after-save", "tenant-a", TypeDocEvent, "Invoice", EventAfterSave, "", "", 10, true, "", 5000, "return true", nil, nil, "", "", created, created))

	got, err := store.LoadActiveScriptsTx(context.Background(), tx, "tenant-a", "Invoice", "")
	if err != nil {
		t.Fatal("load active scripts in source transaction:", err)
	}
	if len(got) != 1 || got[0].Name != "after-save" || got[0].Script != "return true" {
		t.Fatalf("loaded scripts = %+v, want frozen after-save record", got)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit source transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestLoadActiveScriptsTxRejectsMissingTransaction(t *testing.T) {
	store := &Store{Dialect: kdb.Resolve("mysql")}
	if _, err := store.LoadActiveScriptsTx(context.Background(), nil, "tenant-a", "Invoice", ""); err == nil {
		t.Fatal("expected missing transaction to be rejected")
	}
}
