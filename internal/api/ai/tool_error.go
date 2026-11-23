package ai

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

// Structured tool error codes. Every record and confirmation tool error uses one of these,
// so the model can see what is wrong and how to fix it.
const (
	errMissingFields         = "missing_fields"
	errInvalidValue          = "invalid_value"
	errNotFound              = "not_found"
	errPermissionDenied      = "permission_denied"
	errModuleCommandRequired = "module_command_required"
	errGuardFailed           = "guard_failed"
	errConfirmExpired        = "confirm_expired"
	errConfirmInvalid        = "confirm_invalid"
	errInvalidArguments      = "invalid_arguments"
)

// maxValidValues caps the closest options a structured error lists.
const maxValidValues = 5

// aiToolError is a refused tool call. It is returned to the model as {"error":{...}}.
type aiToolError struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	Field       string   `json:"field,omitempty"`
	Fix         string   `json:"fix,omitempty"`
	ValidValues []string `json:"valid_values,omitempty"`
}

func (e aiToolError) Error() string { return e.Code + ": " + e.Message }

// text returns the model-facing form of the error.
func (e aiToolError) text() string {
	data, err := json.Marshal(map[string]any{"error": e})
	if err != nil {
		return `{"error":{"code":"` + errInvalidValue + `","message":"Tool error could not be encoded."}}`
	}
	return string(data)
}

// limitValues keeps at most maxValidValues options.
func limitValues(values []string) []string {
	if len(values) > maxValidValues {
		return values[:maxValidValues]
	}
	return values
}

// recordWriteErrorText maps an error from a kernel record command to its structured form.
// A validation error lists every missing required field in one error; other validation
// errors name their field; a command refusal without field detail is a guard failure.
func recordWriteErrorText(err error) string {
	var cerr *contract.Error
	if !errors.As(err, &cerr) {
		return aiToolError{Code: errInvalidValue, Message: "The record could not be saved."}.text()
	}
	switch {
	case cerr.Type == contract.CodePermissionDenied || cerr.Type == contract.CodeUnauthenticated:
		return aiToolError{Code: errPermissionDenied, Message: cerr.Message}.text()
	case cerr.Type == contract.CodeNotFound:
		return aiToolError{Code: errNotFound, Message: cerr.Message}.text()
	case cerr.Type == contract.CodeValidationFailed:
		var issues doctype.ValidationErrors
		if len(cerr.Details) > 0 && json.Unmarshal(cerr.Details, &issues) == nil && len(issues) > 0 {
			return validationIssuesError(issues).text()
		}
		return aiToolError{Code: errGuardFailed, Message: cerr.Message}.text()
	case cerr.Type == contract.CodeConflict:
		return aiToolError{Code: errInvalidValue, Message: cerr.Message, Field: cerr.Field}.text()
	}
	return aiToolError{Code: errGuardFailed, Message: cerr.Message}.text()
}

// validationIssuesError folds validation issues into one error. Required-field issues are
// listed together under missing_fields; the first other issue becomes invalid_value.
func validationIssuesError(issues doctype.ValidationErrors) aiToolError {
	var missing []string
	var missingFields []string
	for _, issue := range issues {
		if strings.HasSuffix(issue.Message, " is required.") {
			missing = append(missing, strings.TrimSuffix(issue.Message, " is required."))
			missingFields = append(missingFields, issue.Field)
		}
	}
	if len(missing) > 0 {
		return aiToolError{
			Code:    errMissingFields,
			Message: "Missing required fields: " + strings.Join(missing, ", ") + ".",
			Field:   missingFields[0],
			Fix:     "Ask the person for these values, then call create_record again.",
		}
	}
	first := issues[0]
	return aiToolError{Code: errInvalidValue, Message: first.Message, Field: first.Field, Fix: "Correct the value and call the tool again."}
}
