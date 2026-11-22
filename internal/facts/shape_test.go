package facts

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestDecideShape(t *testing.T) {
	base := []doctype.FactColumn{{Name: "register", Type: doctype.FactColumnText}, {Name: "total", Type: doctype.FactColumnDecimal}}
	g := shapeGenerationPrefix
	withCopy := append(append([]doctype.FactColumn{}, base...), doctype.FactColumn{Name: "line_copy", Type: doctype.FactColumnDecimal})
	tests := []struct {
		name        string
		stored      *storedShape
		fingerprint string
		want        []doctype.FactColumn
		action      shapeAction
	}{
		{"never synced", nil, g + "f2", base, shapeRecreate},
		{"same fingerprint", &storedShape{g + "f1", base}, g + "f1", base, shapeKeep},
		{"new measure only", &storedShape{g + "f1", base}, g + "f2", withCopy, shapeAdd},
		{"fingerprint change, same columns", &storedShape{g + "f1", base}, g + "f2", base, shapeAdd},
		{"column removed", &storedShape{g + "f1", base}, g + "f2", base[:1], shapeRecreate},
		{"column retyped", &storedShape{g + "f1", base}, g + "f2",
			[]doctype.FactColumn{{Name: "register", Type: doctype.FactColumnDecimal}, {Name: "total", Type: doctype.FactColumnDecimal}}, shapeRecreate},
		// Rows written by an older generation may hold line rows under an empty id: recreate, even when the columns match.
		{"older generation, same columns", &storedShape{"f1", base}, g + "f1", base, shapeRecreate},
		{"claimed, never synced", &storedShape{"", base}, g + "f1", base, shapeRecreate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideShape(tc.stored, tc.fingerprint, tc.want); got != tc.action {
				t.Fatalf("decideShape = %d, want %d", got, tc.action)
			}
		})
	}
}

func TestModelShapeFingerprint(t *testing.T) {
	withLineCopy := saleFacts()
	withLineCopy.Facts.Measures = append(withLineCopy.Facts.Measures, doctype.FactMeasure{Field: "items.amount", Name: "line_copy"})
	otherTime := saleFacts()
	otherTime.Facts.Time = "creation"
	tests := []struct {
		name    string
		dt      *doctype.DocType
		changed bool
	}{
		{"unchanged definition", saleFacts(), false},
		{"measure added", withLineCopy, true},
		{"time axis moved", otherTime, true},
	}
	base, _, err := modelShape(saleFacts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(base, shapeGenerationPrefix) {
		t.Fatalf("fingerprint %q lacks the shape generation", base)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := modelShape(tc.dt)
			if err != nil {
				t.Fatal(err)
			}
			if (got != base) != tc.changed {
				t.Fatalf("fingerprint changed = %t, want %t", got != base, tc.changed)
			}
		})
	}
}
