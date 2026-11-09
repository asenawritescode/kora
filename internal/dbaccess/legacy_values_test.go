package dbaccess

import (
	"database/sql/driver"
	"errors"
	"testing"
)

func TestLegacyValues(t *testing.T) {
	tests := []struct {
		name    string
		args    []driver.NamedValue
		want    []driver.Value
		wantErr error
	}{
		{name: "positional values", args: []driver.NamedValue{{Ordinal: 1, Value: int64(7)}, {Ordinal: 2, Value: "x"}}, want: []driver.Value{int64(7), "x"}},
		{name: "NULL parameter is a valid nil value", args: []driver.NamedValue{{Ordinal: 1, Value: nil}}, want: []driver.Value{nil}},
		{name: "named parameter cannot use the legacy interface", args: []driver.NamedValue{{Name: "id", Ordinal: 1, Value: int64(1)}}, wantErr: driver.ErrSkip},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := legacyValues(test.args)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("legacyValues() error = %v, want %v", err, test.wantErr)
			}
			if len(got) != len(test.want) {
				t.Fatalf("legacyValues() = %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("legacyValues()[%d] = %v, want %v", i, got[i], test.want[i])
				}
			}
		})
	}
}
