package metrics

import "testing"

// TestPackLabel reads the optional display label of a pack.yaml and treats its absence as no label.
func TestPackLabel(t *testing.T) {
	cases := []struct {
		name, body, want string
		wantErr          bool
	}{
		{"labelled", "name: erpsell\nlabel: Selling\nversion: \"6\"\n", "Selling", false},
		{"no label", "name: erpsell\nversion: \"6\"\n", "", false},
		{"malformed", "label: [unclosed\n", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PackLabel([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v; want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("label = %q; want %q", got, tc.want)
			}
		})
	}
}
