package facts

import (
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestDecideShape(t *testing.T) {
	base := []doctype.FactColumn{{Name: "register", Type: doctype.FactColumnText}, {Name: "total", Type: doctype.FactColumnDecimal}}
	tests := []struct {
		name        string
		stored      *storedShape
		fingerprint string
		want        []doctype.FactColumn
		action      shapeAction
	}{
		{"never synced", nil, "f2", base, shapeRecreate},
		{"same fingerprint", &storedShape{"f1", base}, "f1", base, shapeKeep},
		{"new measure only", &storedShape{"f1", base}, "f2",
			append(append([]doctype.FactColumn{}, base...), doctype.FactColumn{Name: "line_copy", Type: doctype.FactColumnDecimal}), shapeAdd},
		{"fingerprint change, same columns", &storedShape{"f1", base}, "f2", base, shapeAdd},
		{"column removed", &storedShape{"f1", base}, "f2", base[:1], shapeRecreate},
		{"column retyped", &storedShape{"f1", base}, "f2",
			[]doctype.FactColumn{{Name: "register", Type: doctype.FactColumnDecimal}, {Name: "total", Type: doctype.FactColumnDecimal}}, shapeRecreate},
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
