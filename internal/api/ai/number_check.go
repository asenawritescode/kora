package ai

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// numberToken is one number as written in a reply: digits with optional thousands separators and a decimal part.
var numberToken = regexp.MustCompile(`\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?`)

// numberTolerance is how far a number in a reply may sit from a figure the turn produced. Figures are compared as
// the card prints them, so the tolerance only absorbs floating-point noise.
const numberTolerance = 1e-6

// numberSet is the figures a reply may state: every number the turn's results, cards and the user's message
// contain, with the whole and one- and two-decimal forms of each.
type numberSet []float64

// addText adds every number written in text, as the card or the message prints it.
func (s *numberSet) addText(text string) {
	for _, token := range numberToken.FindAllString(text, -1) {
		if v, ok := parseNumber(token); ok {
			s.add(v)
		}
	}
}

// addFigure adds a figure from a result and its rounded forms: a reply may print 16,445 for 16,445.4.
func (s *numberSet) addFigure(v float64) {
	s.add(v)
	s.add(math.Round(v))
	s.add(math.Round(v*10) / 10)
	s.add(math.Round(v*100) / 100)
}

func (s *numberSet) add(v float64) {
	*s = append(*s, v)
}

// allows reports whether v is one of the figures.
func (s numberSet) allows(v float64) bool {
	for _, a := range s {
		if math.Abs(a-v) < numberTolerance {
			return true
		}
	}
	return false
}

// parseNumber reads a number as written, with thousands separators removed.
func parseNumber(token string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(token, ",", ""), 64)
	return v, err == nil
}

// unmatchedNumbers lists the numbers in reply that the set does not allow, in the order they appear. A number
// followed by k or m is checked as written in thousands or millions.
func unmatchedNumbers(reply string, allowed numberSet) []string {
	var out []string
	for _, loc := range numberToken.FindAllStringIndex(reply, -1) {
		token := reply[loc[0]:loc[1]]
		v, ok := parseNumber(token)
		if !ok {
			out = append(out, token)
			continue
		}
		if loc[1] < len(reply) {
			switch reply[loc[1]] {
			case 'k', 'K':
				v *= 1e3
				token += reply[loc[1] : loc[1]+1]
			case 'm', 'M':
				v *= 1e6
				token += reply[loc[1] : loc[1]+1]
			}
		}
		if !allowed.allows(v) {
			out = append(out, token)
		}
	}
	return out
}

// correctionFor is the instruction that sends a reply back once, naming the numbers it may not state.
func correctionFor(unmatched []string) string {
	return fmt.Sprintf("Your reply states numbers that are not in the query results: %s. Reply again using only figures from the query results, or give no number.", strings.Join(unmatched, ", "))
}
