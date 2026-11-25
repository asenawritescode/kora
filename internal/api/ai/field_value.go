package ai

import (
	"reflect"
	"time"

	"github.com/asenawritescode/kora/internal/numeric"
)

// fieldValue is one field value as a preview compares it: the value read back from the database
// and the value the kernel returns. The same value can arrive as different Go types (an int, a
// float64, DECIMAL text), so values compare by meaning, not by type.
type fieldValue struct{ v any }

// same reports whether a and b hold the same field value.
func (a fieldValue) same(b fieldValue) bool {
	if a.blank() || b.blank() {
		return a.blank() && b.blank()
	}
	if x, ok := numeric.Parse(a.v); ok {
		y, ok := numeric.Parse(b.v)
		return ok && x.Equal(y)
	}
	if x, ok := a.v.(time.Time); ok {
		y, ok := b.v.(time.Time)
		return ok && x.Equal(y)
	}
	return reflect.DeepEqual(a.v, b.v)
}

// blank reports whether the value is empty: nil or the empty text.
func (a fieldValue) blank() bool {
	if a.v == nil {
		return true
	}
	s, ok := a.v.(string)
	return ok && s == ""
}
