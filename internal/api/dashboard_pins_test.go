package api

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestWithPinRespectsLimit(t *testing.T) {
	full := []contract.DashboardPin{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	tests := []struct {
		name    string
		pins    []contract.DashboardPin
		wantErr bool
		want    int
	}{
		{"empty dashboard", nil, false, 1},
		{"below the limit", full[:3], false, 4},
		{"at the limit", full, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withPin(tc.pins, contract.DashboardPin{ID: "new"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if len(got) != tc.want {
				t.Fatalf("len = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestWithoutPin(t *testing.T) {
	pins := []contract.DashboardPin{{ID: "a"}, {ID: "b"}}
	tests := []struct {
		name    string
		id      string
		wantErr bool
		want    []string
	}{
		{"known pin", "a", false, []string{"b"}},
		{"unknown pin", "zz", true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withoutPin(pins, tc.id)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if len(got) != len(tc.want) || (len(got) == 1 && got[0].ID != tc.want[0]) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if len(pins) != 2 {
				t.Fatal("input mutated")
			}
		})
	}
}

func TestDecodeEncodePins(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"column added to an existing table", "", "[]"},
		{"empty array", "[]", "[]"},
		{"one pin", `[{"id":"p1","label":"Sales","question":{"record_type":"Sale"}}]`, `[{"id":"p1","label":"Sales","question":{"record_type":"Sale"}}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pins, err := decodePins(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := encodePins(pins)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("encoded = %s, want %s", got, tc.want)
			}
		})
	}
	if _, err := decodePins("{"); err == nil {
		t.Fatal("decode accepted broken JSON")
	}
}
