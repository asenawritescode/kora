package ai

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/numeric"
)

// fieldValue is one field value as a preview compares it: the value read back from the database
// and the value the kernel returns. The same number can arrive as different Go types (an int, a
// float64, DECIMAL text), so a numeric field compares by value. Text in a text field stays text:
// "0722695333" and "722695333" are different phone numbers.
type fieldValue struct {
	v      any
	number bool // the field holds a number, so numeric text compares by value
}

// previewValue returns the value of field fieldname of dt, marked numeric when the field is.
func previewValue(dt *doctype.DocType, fieldname string, v any) fieldValue {
	f := dt.GetField(fieldname)
	if f == nil {
		return fieldValue{v: v}
	}
	switch strings.ToLower(f.Fieldtype) {
	case "int", "float", "currency", "percent", "check":
		return fieldValue{v: v, number: true}
	}
	return fieldValue{v: v}
}

// same reports whether a and b hold the same field value.
func (a fieldValue) same(b fieldValue) bool {
	if a.blank() || b.blank() {
		return a.blank() && b.blank()
	}
	if a.number || b.number || (a.goNumber() && b.goNumber()) {
		x, okA := numeric.Parse(a.v)
		y, okB := numeric.Parse(b.v)
		if okA && okB {
			return x.Equal(y)
		}
	}
	if x, ok := a.v.(time.Time); ok {
		y, ok := b.v.(time.Time)
		return ok && x.Equal(y)
	}
	return reflect.DeepEqual(a.v, b.v)
}

// goNumber reports whether the value is a Go number rather than text.
func (a fieldValue) goNumber() bool {
	switch a.v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return true
	}
	return false
}

// blank reports whether the value is empty: nil or the empty text.
func (a fieldValue) blank() bool {
	if a.v == nil {
		return true
	}
	s, ok := a.v.(string)
	return ok && s == ""
}
