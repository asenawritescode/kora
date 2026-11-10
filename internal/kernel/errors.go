package kernel

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

// Typed sentinel errors. Adapters map these to wire errors via contract.Code;
// they must never be detected by string matching.
var (
	ErrUnauthenticated = contract.NewError(contract.CodeUnauthenticated, "operation context has no authenticated actor")
	ErrNoSite          = contract.NewError(contract.CodeValidationFailed, "operation context missing tenant site")
	ErrPermission      = contract.NewError(contract.CodePermissionDenied, "not permitted")
	ErrStaleVersion    = contract.NewError(contract.CodeConflict, "document was modified by another operation")
	ErrKeyReused       = contract.NewError(contract.CodeIdempotencyKeyReused, "idempotency key already used with a different payload")
)

// wrapORMError converts ORM sentinel errors into typed contract errors.
func wrapORMError(err error) *contract.Error {
	if errors.Is(err, orm.ErrConflict) {
		return ErrStaleVersion
	}
	switch {
	case errors.Is(err, orm.ErrDuplicate):
		return contract.NewError(contract.CodeConflict, sanitize(err.Error()))
	case errors.Is(err, orm.ErrValidation):
		return contract.NewError(contract.CodeValidationFailed, "record validation failed")
	case errors.Is(err, orm.ErrNotFound):
		return contract.NewError(contract.CodeNotFound, "document not found or access denied")
	default:
		return contract.NewError(contract.CodeDependencyUnavailable, "record persistence failed")
	}
}

func wrapExpectedVersionORMError(err error, op Operation, dialect db.Dialect) *contract.Error {
	if expectedVersionFrom(op) != "" && dialect.IsWriteConflict(err) {
		return ErrStaleVersion
	}
	return wrapORMError(err)
}

// sanitize strips SQL driver internals from user-facing messages while keeping
// the meaningful suffix.
func sanitize(msg string) string {
	if i := strings.LastIndex(msg, ": "); i > 0 && strings.Count(msg, ": ") > 1 {
		return msg[i+2:]
	}
	return msg
}

func (k *Kernel) rejected(opID string, op Operation, e *contract.Error) (contract.CommandResult, *contract.Error) {
	return contract.CommandResult{
		OperationID:   opID,
		CorrelationID: op.Context.CorrelationID,
		Status:        statusFor(e.Type),
		Error:         e,
	}, e
}

func statusFor(c contract.Code) contract.Status {
	switch c {
	case contract.CodeConflict:
		return contract.StatusConflict
	case contract.CodePermissionDenied, contract.CodeUnauthenticated,
		contract.CodeValidationFailed, contract.CodeNotFound, contract.CodeIdempotencyKeyReused:
		return contract.StatusRejected
	default:
		return contract.StatusFailed
	}
}

func validationContractError(verrs doctype.ValidationErrors) *contract.Error {
	raw, _ := json.Marshal(verrs)
	e := contract.NewError(contract.CodeValidationFailed, "validation failed")
	e.Details = raw
	return e
}
