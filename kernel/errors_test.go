package kernel

import (
	"errors"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/orm"
)

func TestWrapORMErrorClassifiesPersistenceFailuresWithoutLeakingDriverText(t *testing.T) {
	cerr := wrapORMError(errors.New("postgres: relation private_table failed; password=secret"))
	if cerr.Type != contract.CodeDependencyUnavailable {
		t.Fatalf("error type = %s, want %s", cerr.Type, contract.CodeDependencyUnavailable)
	}
	if cerr.Message != "record persistence failed" {
		t.Fatalf("error message = %q, want generic persistence message", cerr.Message)
	}
}

func TestWrapORMValidationErrorDoesNotReportDependencyOutage(t *testing.T) {
	cerr := wrapORMError(errors.Join(orm.ErrValidation, errors.New("Sale Item.unit_price is required")))
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
