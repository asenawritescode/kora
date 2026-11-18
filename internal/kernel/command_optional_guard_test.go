package kernel

import "testing"

func TestOptionalMatchesNeedNoInputWhenTheirReferenceIsAbsent(t *testing.T) {
	where := map[string]string{"name": "$input.sale_number", "status": "Paid"}
	env := refEnv{input: map[string]any{}}
	cases := []struct {
		name       string
		match      *GuardMatch
		wantAbsent bool
		wantRefs   bool
	}{
		{"required match reports its absent input; it is still required", &GuardMatch{Where: where}, true, true},
		{"optional match skips when its input is absent", &GuardMatch{Where: where, Optional: true}, true, false},
		{"absent match is nothing to require", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.match != nil {
				if got := absentRef(tc.match, env); got != tc.wantAbsent {
					t.Fatalf("absentRef = %v, want %v", got, tc.wantAbsent)
				}
			}
			if got := requiredMatchRefs(tc.match) != nil; got != tc.wantRefs {
				t.Fatalf("requiredMatchRefs present = %v, want %v", got, tc.wantRefs)
			}
		})
	}
}
