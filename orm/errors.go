package orm

import "errors"

// Sentinel errors returned by ORM operations.
// Callers should use errors.Is to check for these.
var (
	// ErrNotFound is returned when a document is not found.
	ErrNotFound = errors.New("document not found")
	// ErrDuplicate is returned on unique constraint violation.
	ErrDuplicate = errors.New("duplicate document")
	// ErrValidation is returned when document validation fails.
	ErrValidation = errors.New("validation error")
)
