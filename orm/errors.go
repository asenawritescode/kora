package orm

import "errors"

var (
	ErrNotFound   = errors.New("document not found")
	ErrDuplicate  = errors.New("duplicate document")
	ErrValidation = errors.New("validation error")
)
