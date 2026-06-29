package doctype

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ValidationError represents a single constraint violation.
type ValidationError struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Field     string `json:"field,omitempty"`
	DocType   string `json:"doctype"`
	Predicate string `json:"predicate,omitempty"` // s-expression predicate that failed
}

// Error implements the error interface.
func (e *ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s (field: %s)", e.Type, e.Message, e.Field)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// ValidationErrors is a collection of validation errors.
type ValidationErrors []*ValidationError

// Error implements the error interface.
func (ve ValidationErrors) Error() string {
	if len(ve) == 0 {
		return ""
	}
	if len(ve) == 1 {
		return ve[0].Error()
	}
	return fmt.Sprintf("%d validation errors: %s", len(ve), ve[0].Message)
}

// HasErrors returns true if there are any validation errors.
func (ve ValidationErrors) HasErrors() bool {
	return len(ve) > 0
}

// ValidateDocument runs all field-level and document-level constraints against a document.
// Returns all violations as a ValidationErrors slice.
func ValidateDocument(dt *DocType, doc *Document, registry *Registry, oldDoc *Document) ValidationErrors {
	var errors ValidationErrors

	// Evaluate predicate-based document constraints.
	sandbox := NewLispSandbox()
	defer sandbox.Close()

	for _, dc := range dt.DocConstraints {
		if dc.Predicate == "" {
			continue
		}
		if dc.Condition != "" {
			if !evaluateCondition(dc.Condition, doc, nil) {
				continue
			}
		}

		result, err := sandbox.Eval(dc.Predicate, doc.Fields, nil)
		if err != nil {
			errors = append(errors, &ValidationError{
				Type:    "ValidationError",
				Message: fmt.Sprintf("Predicate evaluation error: %v", err),
				DocType: dt.Name,
			})
			continue
		}

		// Predicate passes if non-nil and non-zero.
		if !predicateResult(result) {
			msg := dc.Message
			if msg == "" {
				msg = fmt.Sprintf("Predicate %q failed.", dc.Predicate)
			}
			errors = append(errors, &ValidationError{
				Type:    "ValidationError",
				Message: msg,
				DocType: dt.Name,
					Predicate: dc.Predicate,
			})
		}
	}

	for _, field := range dt.Fields {
		if !field.IsDataField() || field.Fieldtype == "Table" {
			continue
		}

		val := doc.Get(field.Fieldname)

		// Check required.
		if field.Reqd {
			if isNilOrEmpty(val) {
				// Check if conditionally required.
				if field.MandatoryDependsOn != "" {
					if evaluateCondition(field.MandatoryDependsOn, doc, nil) {
						errors = append(errors, &ValidationError{
							Type:    "ValidationError",
							Message: fmt.Sprintf("%s is required.", field.Label),
							Field:   field.Fieldname,
							DocType: dt.Name,
						})
					}
				} else {
					errors = append(errors, &ValidationError{
						Type:    "ValidationError",
						Message: fmt.Sprintf("%s is required.", field.Label),
						Field:   field.Fieldname,
						DocType: dt.Name,
					})
				}
			}
		}

		// Skip further validation if value is empty and not required.
		if isNilOrEmpty(val) {
			continue
		}

		// Run constraint validations.
		for _, c := range field.Constraints {
			if c.Condition != "" {
				if !evaluateCondition(c.Condition, doc, nil) {
					continue // Constraint is conditional and condition is false.
				}
			}

			err := validateConstraint(&field, &c, val, dt, doc, registry)
			if err != nil {
				errors = append(errors, err)
			}
		}
	}

	// Run document-level constraints.
	for _, dc := range dt.DocConstraints {
		if dc.Condition != "" {
			if !evaluateCondition(dc.Condition, doc, nil) {
				continue
			}
		}

		docErrs := validateDocConstraint(&dc, dt, doc, oldDoc)
		errors = append(errors, docErrs...)
	}

	return errors
}

func validateConstraint(field *Field, c *Constraint, val any, dt *DocType, doc *Document, registry *Registry) *ValidationError {
	switch c.Type {
	case "min":
		return validateMin(field, c, val, dt)
	case "max":
		return validateMax(field, c, val, dt)
	case "min_length":
		return validateMinLength(field, c, val, dt)
	case "max_length":
		return validateMaxLength(field, c, val, dt)
	case "min_date":
		return validateMinDate(field, c, val, dt)
	case "max_date":
		return validateMaxDate(field, c, val, dt)
	case "min_rows":
		return validateMinRows(field, c, val, dt)
	case "max_rows":
		return validateMaxRows(field, c, val, dt)
	case "regex":
		return validateRegex(field, c, val, dt)
	case "one_of":
		return validateOneOf(field, c, val, dt)
	case "not_one_of":
		return validateNotOneOf(field, c, val, dt)
	case "exists":
		return validateExists(field, c, val, dt, registry)
	case "unique_in":
		return validateUniqueIn(field, c, val, dt)
	case "required_if":
		return validateRequiredIf(field, c, val, dt, doc)
	default:
		return &ValidationError{
			Type:    "ConstraintError",
			Message: fmt.Sprintf("Unknown constraint type: %s", c.Type),
			Field:   field.Fieldname,
			DocType: dt.Name,
		}
	}
}

func validateMin(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	minVal, ok := toFloat64(c.Value)
	if !ok {
		return nil
	}
	actual, ok := toFloat64(val)
	if !ok {
		return nil
	}
	if actual < minVal {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s must be at least %v.", field.Label, c.Value)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateMax(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	maxVal, ok := toFloat64(c.Value)
	if !ok {
		return nil
	}
	actual, ok := toFloat64(val)
	if !ok {
		return nil
	}
	if actual > maxVal {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s must be at most %v.", field.Label, c.Value)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateMinLength(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	minLen, ok := toInt(c.Value)
	if !ok {
		return nil
	}
	s := fmt.Sprintf("%v", val)
	if len(s) < minLen {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s must be at least %d characters.", field.Label, minLen)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateMaxLength(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	maxLen, ok := toInt(c.Value)
	if !ok {
		return nil
	}
	s := fmt.Sprintf("%v", val)
	if len(s) > maxLen {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s must be at most %d characters.", field.Label, maxLen)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateMinDate(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	minDate, err := resolveDateValue(c.Value)
	if err != nil {
		return nil
	}
	dateVal, err := parseDate(val)
	if err != nil {
		return nil
	}
	if dateVal.Before(minDate) {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s cannot be before %s.", field.Label, minDate.Format("2006-01-02"))
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateMaxDate(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	maxDate, err := resolveDateValue(c.Value)
	if err != nil {
		return nil
	}
	dateVal, err := parseDate(val)
	if err != nil {
		return nil
	}
	if dateVal.After(maxDate) {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s cannot be after %s.", field.Label, maxDate.Format("2006-01-02"))
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateMinRows(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	minRows, ok := toInt(c.Value)
	if !ok {
		return nil
	}
	docs, ok := val.([]*Document)
	if !ok {
		return nil
	}
	if len(docs) < minRows {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s must have at least %d rows.", field.Label, minRows)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateMaxRows(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	maxRows, ok := toInt(c.Value)
	if !ok {
		return nil
	}
	docs, ok := val.([]*Document)
	if !ok {
		return nil
	}
	if len(docs) > maxRows {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s must have at most %d rows.", field.Label, maxRows)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateRegex(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	r, err := regexp.Compile(c.Pattern)
	if err != nil {
		return nil
	}
	s := fmt.Sprintf("%v", val)
	if !r.MatchString(s) {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s does not match the required pattern.", field.Label)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateOneOf(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	s := fmt.Sprintf("%v", val)
	for _, allowed := range c.Values {
		if s == allowed {
			return nil
		}
	}
	msg := c.Message
	if msg == "" {
		msg = fmt.Sprintf("%s must be one of: %s.", field.Label, strings.Join(c.Values, ", "))
	}
	return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
}

func validateNotOneOf(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	s := fmt.Sprintf("%v", val)
	for _, forbidden := range c.Values {
		if s == forbidden {
			msg := c.Message
			if msg == "" {
				msg = fmt.Sprintf("%s cannot be %s.", field.Label, forbidden)
			}
			return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
		}
	}
	return nil
}

func validateExists(field *Field, c *Constraint, val any, dt *DocType, registry *Registry) *ValidationError {
	// This is a soft check — the actual existence check requires a DB query.
	// We verify the linked doctype exists in the registry.
	s := fmt.Sprintf("%v", val)
	if s == "" {
		return nil
	}
	targetDT := registry.Get(field.Options)
	if targetDT == nil {
		return &ValidationError{
			Type:    "ConstraintError",
			Message: fmt.Sprintf("Link target doctype %q not found in registry.", field.Options),
			Field:   field.Fieldname,
			DocType: dt.Name,
		}
	}
	// The actual row existence check is done at the ORM level.
	_ = targetDT
	return nil
}

func validateUniqueIn(field *Field, c *Constraint, val any, dt *DocType) *ValidationError {
	// Unique constraint enforcement is done at the DB level (unique index).
	// This is a placeholder for future client-side hinting.
	return nil
}

func validateRequiredIf(field *Field, c *Constraint, val any, dt *DocType, doc *Document) *ValidationError {
	if !evaluateCondition(c.Condition, doc, nil) {
		return nil
	}
	if isNilOrEmpty(val) {
		msg := c.Message
		if msg == "" {
			msg = fmt.Sprintf("%s is required when %s.", field.Label, c.Condition)
		}
		return &ValidationError{Type: "ValidationError", Message: msg, Field: field.Fieldname, DocType: dt.Name}
	}
	return nil
}

func validateDocConstraint(dc *DocConstraint, dt *DocType, doc *Document, oldDoc *Document) ValidationErrors {
	var errors ValidationErrors

	switch dc.Type {
	case "field_dependency":
		for _, fieldName := range dc.RequireFields {
			val := doc.Get(fieldName)
			if isNilOrEmpty(val) {
				field := dt.GetField(fieldName)
				label := fieldName
				if field != nil {
					label = field.Label
				}
				errors = append(errors, &ValidationError{
					Type:    "ValidationError",
					Message: dc.Message,
					Field:   fieldName,
					DocType: dt.Name,
				})
				_ = label
			}
		}

	case "cross_field":
		lhs := doc.Get(dc.LHS)
		rhs := doc.Get(dc.RHS)
		if lhs == nil || rhs == nil {
			break
		}
		switch dc.Operator {
		case ">=":
			if compareValues(lhs, rhs) < 0 {
				errors = append(errors, &ValidationError{
					Type:    "ValidationError",
					Message: dc.Message,
					DocType: dt.Name,
				})
			}
		case "<=":
			if compareValues(lhs, rhs) > 0 {
				errors = append(errors, &ValidationError{
					Type:    "ValidationError",
					Message: dc.Message,
					DocType: dt.Name,
				})
			}
		case ">":
			if compareValues(lhs, rhs) <= 0 {
				errors = append(errors, &ValidationError{
					Type:    "ValidationError",
					Message: dc.Message,
					DocType: dt.Name,
				})
			}
		case "<":
			if compareValues(lhs, rhs) >= 0 {
				errors = append(errors, &ValidationError{
					Type:    "ValidationError",
					Message: dc.Message,
					DocType: dt.Name,
				})
			}
		case "==":
			if !valuesEqual(lhs, rhs) {
				errors = append(errors, &ValidationError{
					Type:    "ValidationError",
					Message: dc.Message,
					DocType: dt.Name,
				})
			}
		}

	case "immutable_after":
		if oldDoc == nil {
			break
		}
		statusVal := doc.Get(dc.StatusField)
		statusStr := fmt.Sprintf("%v", statusVal)
		for _, s := range dc.StatusValues {
			if statusStr == s {
				for _, fieldName := range dc.ImmutableFields {
					oldVal := oldDoc.Get(fieldName)
					newVal := doc.Get(fieldName)
					if !valuesEqual(oldVal, newVal) {
						field := dt.GetField(fieldName)
						label := fieldName
						if field != nil {
							label = field.Label
						}
						errors = append(errors, &ValidationError{
							Type:    "ValidationError",
							Message: fmt.Sprintf("%s cannot be changed when status is %s.", label, s),
							Field:   fieldName,
							DocType: dt.Name,
						})
					}
				}
			}
		}
	}

	return errors
}

// --- Utility functions ---

func isNilOrEmpty(val any) bool {
	if val == nil {
		return true
	}
	switch v := val.(type) {
	case string:
		return v == ""
	case []byte:
		return len(v) == 0
	case []any:
		return len(v) == 0
	case []*Document:
		return len(v) == 0
	}
	return false
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(n, "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		var i int
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}

func resolveDateValue(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("date value must be a string")
	}
	s = strings.TrimSpace(s)
	if s == "today" {
		now := time.Now()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), nil
	}
	if strings.HasPrefix(s, "today+") {
		var days int
		fmt.Sscanf(s, "today+%d", &days)
		now := time.Now()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, days), nil
	}
	// Try ISO date format.
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

func parseDate(v any) (time.Time, error) {
	switch val := v.(type) {
	case time.Time:
		return val, nil
	case string:
		t, err := time.Parse("2006-01-02", val)
		if err != nil {
			// Try datetime format.
			t, err = time.Parse("2006-01-02T15:04:05", val)
			if err != nil {
				return time.Time{}, err
			}
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot parse date from %v", v)
}

func compareValues(a, b any) int {
	aStr := fmt.Sprintf("%v", a)
	bStr := fmt.Sprintf("%v", b)
	return strings.Compare(aStr, bStr)
}

func valuesEqual(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// predicateResult returns true if the predicate evaluation result is truthy.
// A nil or zero result means the predicate failed.
func predicateResult(result any) bool {
	if result == nil {
		return false
	}
	f, ok := result.(float64)
	if ok {
		return f > 0
	}
	// Non-nil, non-float64 results (strings, etc.) are considered truthy.
	return true
}

