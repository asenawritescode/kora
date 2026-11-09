package script

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
)

func TestScriptInsertAndConfigRevisionCommitTogether(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	store := &Store{DB: database, Dialect: db.MySQL()}
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO _kora_script").WithArgs(
		"order_hook", "tenant-a", "doc_event", "Order", "before_save", "", "", 0, true, "", 1000,
		"return document", "operator-a", "operator-a",
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE _kora_config_revision SET revision = revision \+ 1 WHERE id = 1 AND revision < 9223372036854775807`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.Insert(ScriptRecord{
		Name: "order_hook", Site: "tenant-a", ScriptType: TypeDocEvent, DocType: "Order", Event: EventBeforeSave,
		IsActive: true, TimeoutMs: 1000, Script: "return document", CreatedBy: "operator-a", UpdatedBy: "operator-a",
	}); err != nil {
		t.Fatal("insert script:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
