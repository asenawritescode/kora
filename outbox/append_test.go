package outbox

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
)

func TestSQLWriterAppendCompletesEnvelopeDefaultsBeforeValidation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO _kora_outbox`).
		WithArgs(sqlmock.AnyArg(), "live-test", "record.created", 1, "Record", "REC-1", sqlmock.AnyArg(), "pending", 0, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = NewSQLWriter(kdb.Resolve("mysql")).Append(context.Background(), tx, contract.EventEnvelope{
		Type: "record.created", Source: "kora.kernel", Site: "live-test",
		AggregateType: "Record", AggregateID: "REC-1",
		Data: contract.MustEncodeData(map[string]any{"name": "REC-1"}),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
