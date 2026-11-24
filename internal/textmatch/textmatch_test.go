package textmatch

import (
	"reflect"
	"testing"
)

func TestWordsSplitsLowerCaseLettersAndDigits(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"Mr. Ouma Son", []string{"mr", "ouma", "son"}},
		{"  SUPP-0001 ", []string{"supp", "0001"}},
		{"%_!", []string{}},
	} {
		if got := Words(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Words(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestLikeEscapeQuotesWildcards(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ouma", "ouma"}, {"50%", "50!%"}, {"a_b", "a!_b"}, {"wow!", "wow!!"},
	} {
		if got := LikeEscape(tc.in); got != tc.want {
			t.Errorf("LikeEscape(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDistanceCountsRunes(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"", "abc", 3}, {"kitten", "sitting", 3},
		{"oumas", "ouma", 1}, {"granery", "granary", 1}, {"ñandu", "nandu", 1},
	} {
		if got := Distance(tc.a, tc.b); got != tc.want {
			t.Errorf("Distance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFuzzyTypoRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		title string
		want  bool
	}{
		{"oumas son matches Mr. Ouma Son", "oumas son", "Mr. Ouma Son", true},
		{"granery matches Granary (7 runes, two edits allowed)", "Granery", "Mr. Ouma Granary", true},
		{"zzz matches nothing", "zzz", "Mr. Ouma Son", false},
		{"one edit on a short word still matches: son vs sun", "son", "Sun Ltd", true},
		{"unrelated short word does not: son vs snack", "son", "Snack Bar", false},
		{"prefix of at least three runes", "patr", "Patrice Ouma", true},
		{"prefix shorter than three runes is not enough", "pa", "Patrice Ouma", false},
		{"every query word must match", "ouma zzz", "Mr. Ouma Son", false},
		{"empty query", "", "Mr. Ouma Son", false},
		{"empty title", "ouma", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Fuzzy(Words(tc.query), Words(tc.title))
			if got != tc.want {
				t.Errorf("Fuzzy(%q, %q) = %v, want %v", tc.query, tc.title, got, tc.want)
			}
		})
	}
}

func TestFuzzyTotalRanksCloser(t *testing.T) {
	_, near := Fuzzy(Words("oumas son"), Words("Mr. Ouma Son"))
	_, far := Fuzzy(Words("oumas son"), Words("Patrice Ouma"))
	if near >= far {
		t.Errorf("closer title total = %d, farther = %d; want closer to rank lower", near, far)
	}
}
