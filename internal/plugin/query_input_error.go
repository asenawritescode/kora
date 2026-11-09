package plugin

import (
	"errors"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

const maxQueryInputFieldRunes = 64

// QueryInputError is a published-query call the caller got wrong: input that
// fails the query's input schema, a missing required input, a page outside
// the registered bounds. Transports report it as a client error. Message is
// bounded and safe to return; the validator's own text is never exposed.
type QueryInputError struct {
	Field   string
	Message string
}

func (e *QueryInputError) Error() string { return e.Message }

// newQueryInputError names the offending input field from a schema failure
// when the validator exposes it; any other cause gets a generic message.
func newQueryInputError(cause error) *QueryInputError { return newInputError(cause, "query") }

// newInputError is newQueryInputError for any published call; subject is the
// word the messages use for it ("query", "operation").
func newInputError(cause error, subject string) *QueryInputError {
	var ve *jsonschema.ValidationError
	if !errors.As(cause, &ve) {
		return &QueryInputError{Message: subject + " input is invalid"}
	}
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	field, msg := "", subject+" input is invalid"
	switch k := ve.ErrorKind.(type) {
	case *kind.Required:
		if len(k.Missing) > 0 {
			field, msg = boundedField(k.Missing[0]), " is required"
		}
	case *kind.AdditionalProperties:
		if len(k.Properties) > 0 {
			field, msg = boundedField(k.Properties[0]), " is not accepted by this "+subject
		}
	default:
		if len(ve.InstanceLocation) > 0 {
			field, msg = boundedField(ve.InstanceLocation[0]), " is invalid"
		}
	}
	if field != "" {
		msg = "input " + field + msg
	}
	return &QueryInputError{Field: field, Message: msg}
}

func boundedField(s string) string {
	if r := []rune(s); len(r) > maxQueryInputFieldRunes {
		return string(r[:maxQueryInputFieldRunes])
	}
	return s
}
