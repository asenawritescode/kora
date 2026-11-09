package api

import (
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	kdb "github.com/asenawritescode/kora/internal/db"
)

func TestRollbackConfigOperationTxRecordsOperation(t *testing.T) {
	cause := errors.New("activation failed")
	op := configOperation{Site: "site-a", ID: "ver-1", Kind: "activate", Phase: "validated", DDLPlan: "[]", RecoveryPlan: "replan"}
	tests := []struct {
		name    string
		execErr error
	}{
		{"journal updated", nil},
		{"journal update fails and the cause is still reported", errors.New("db down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			// Argument order: phase, ddl plan, recovery plan, last error, site, version, kind.
			expect := mock.ExpectExec("UPDATE _kora_config_operation").
				WithArgs(op.Phase, op.DDLPlan, op.RecoveryPlan, cause.Error(), op.Site, op.ID, op.Kind, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg())
			if tt.execErr != nil {
				expect.WillReturnError(tt.execErr)
			} else {
				expect.WillReturnResult(sqlmock.NewResult(0, 1))
			}
			got := rollbackConfigOperationTx(nil, database, kdb.Resolve("mysql"), op, cause)
			if !errors.Is(got, cause) {
				t.Fatalf("error = %v, want it to wrap %v", got, cause)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
