// Package textmatch matches typed text against stored names and titles: word split, LIKE escaping,
// edit distance and typo tolerance.
package textmatch

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Words splits text into lower-case words of letters and digits.
func Words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// LikeEscape escapes a word for a LIKE pattern written with ESCAPE '!'.
func LikeEscape(word string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(word)
}

// Distance is the Levenshtein distance between a and b, counted in runes.
func Distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// Fuzzy reports whether every query word is near some title word, and the sum over the query words of the
// distance to the nearest title word, which ranks titles that do not match.
func Fuzzy(query, title []string) (bool, int) {
	if len(query) == 0 {
		return false, 0
	}
	ok, total := true, 0
	for _, q := range query {
		hit, best := false, -1
		for _, t := range title {
			if d := Distance(q, t); best < 0 || d < best {
				best = d
			}
			hit = hit || near(q, t)
		}
		if best < 0 {
			return false, 0
		}
		ok = ok && hit
		total += best
	}
	return ok, total
}

// near is the typo rule for one word pair: an edit distance of at most 1 for a query word of up to 5 runes and at
// most 2 for a longer one, or one word a prefix of the other when the shorter has at least 3 runes.
func near(q, t string) bool {
	if strings.HasPrefix(t, q) || strings.HasPrefix(q, t) {
		if min(utf8.RuneCountInString(q), utf8.RuneCountInString(t)) >= 3 {
			return true
		}
	}
	allowed := 1
	if utf8.RuneCountInString(q) >= 6 {
		allowed = 2
	}
	return Distance(q, t) <= allowed
}
