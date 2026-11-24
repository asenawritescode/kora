package orm

import (
	"reflect"
	"testing"
)

func TestTypoTitleMatchesRules(t *testing.T) {
	candidates := []TitleMatch{
		{Name: "SUPP-0001", Title: "Mr. Ouma Son"},
		{Name: "SUPP-0002", Title: "Mr. Ouma Granary"},
		{Name: "SUPP-0003", Title: "Patrice Ouma"},
	}
	for _, tc := range []struct {
		name    string
		words   []string
		kind    string
		names   []string
		closest int
	}{
		{name: "oumas son finds Mr. Ouma Son", words: []string{"oumas", "son"}, kind: TitleKindTypo, names: []string{"SUPP-0001"}},
		{name: "granery finds Granary", words: []string{"granery"}, kind: TitleKindTypo, names: []string{"SUPP-0002"}},
		{name: "zzz finds nothing and reports the closest titles", words: []string{"zzz"}, closest: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := typoTitleMatches(candidates, tc.words)
			if got.Kind != tc.kind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tc.kind)
			}
			var names []string
			for _, m := range got.Matches {
				names = append(names, m.Name)
			}
			if tc.names != nil && !reflect.DeepEqual(names, tc.names) {
				t.Errorf("matched names = %q, want %q", names, tc.names)
			}
			if len(got.Closest) != tc.closest {
				t.Errorf("closest = %d, want %d", len(got.Closest), tc.closest)
			}
		})
	}
}
