package sqlstore

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
)

func TestCreateHumanWaitRejectsMissingPreparedRequestWithoutOpeningTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewExecutionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateHumanWait(context.Background(), nil, workflow.StepCommit{})
	if err == nil {
		t.Fatal("missing prepared request was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("missing request unexpectedly touched SQL: %v", err)
	}
}
