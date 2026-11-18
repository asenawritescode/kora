package kernel

import (
	"errors"
	"fmt"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/lib/pq"
)

func TestWrapORMErrorClassifiesPersistenceFailuresWithoutLeakingDriverText(t *testing.T) {
	cerr := wrapORMError(errors.New("postgres: relation private_table failed; password=secret"), &db.MySQLDialect{})
	if cerr.Type != contract.CodeDependencyUnavailable {
		t.Fatalf("error type = %s, want %s", cerr.Type, contract.CodeDependencyUnavailable)
	}
	if cerr.Message != "record persistence failed" {
		t.Fatalf("error message = %q, want generic persistence message", cerr.Message)
	}
}

func TestWrapORMValidationErrorDoesNotReportDependencyOutage(t *testing.T) {
	cerr := wrapORMError(errors.Join(orm.ErrValidation, errors.New("Sale Item.unit_price is required")), &db.MySQLDialect{})
	if cerr.Type != contract.CodeValidationFailed {
		t.Fatalf("error type = %s, want %s", cerr.Type, contract.CodeValidationFailed)
	}
	if cerr.Message != "record validation failed" {
		t.Fatalf("error message = %q, want generic validation message", cerr.Message)
	}
}

func TestExpectedVersionSQLiteContentionMapsToConflict(t *testing.T) {
	op := Operation{Context: OperationContext{ExpectedVersion: "2026-10-02T03:04:05Z"}}
	for _, message := range []string{"database is locked", "SQLITE_BUSY: database is locked"} {
		got := wrapExpectedVersionORMError(errors.New(message), op, &db.LibSQLDialect{})
		if got.Type != contract.CodeConflict {
			t.Errorf("%q mapped to %s, want conflict", message, got.Type)
		}
	}

	withoutExpectedVersion := wrapExpectedVersionORMError(errors.New("database is locked"), Operation{}, &db.LibSQLDialect{})
	if withoutExpectedVersion.Type != contract.CodeDependencyUnavailable {
		t.Fatalf("unversioned lock mapped to %s, want dependency unavailable", withoutExpectedVersion.Type)
	}
	otherFailure := wrapExpectedVersionORMError(errors.New("connection reset"), op, &db.LibSQLDialect{})
	if otherFailure.Type != contract.CodeDependencyUnavailable {
		t.Fatalf("non-contention error mapped to %s, want dependency unavailable", otherFailure.Type)
	}
}

// TestWrapORMErrorFirstDatabaseErrorDecides pins the classification of the
// errors a persistence call can return. A 25P02 (aborted transaction) is never
// a dependency outage: it only follows an earlier failure in the same
// transaction, and that earlier error is the one the caller must see.
func TestWrapORMErrorFirstDatabaseErrorDecides(t *testing.T) {
	aborted := &pq.Error{Code: "25P02", Message: "current transaction is aborted"}
	duplicate := fmt.Errorf("%w: %w", orm.ErrDuplicate, errors.New("serial must be unique."))
	tests := []struct {
		name    string
		err     error
		dialect db.Dialect
		want    contract.Code
	}{
		{"postgres duplicate", duplicate, &db.PostgresDialect{}, contract.CodeConflict},
		{"postgres aborted transaction", fmt.Errorf("inserting document: %w", aborted), &db.PostgresDialect{}, contract.CodeInternal},
		{"postgres unknown failure", errors.New("connection reset"), &db.PostgresDialect{}, contract.CodeDependencyUnavailable},
		{"mysql aborted code is not special", aborted, &db.MySQLDialect{}, contract.CodeDependencyUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wrapORMError(tt.err, tt.dialect).Type; got != tt.want {
				t.Fatalf("classified as %s, want %s", got, tt.want)
			}
		})
	}
}
