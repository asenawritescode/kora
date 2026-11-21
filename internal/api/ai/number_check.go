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

// codeToken is a record code: letters and digits joined by "-" or "/" (PROD-3, ITM-0005, INV/22). The digits in
// a code name a record; they are not quantities, so a reply may repeat them without a figure to back them.
var codeToken = regexp.MustCompile(`[A-Za-z0-9]+(?:[-/][A-Za-z0-9]+)+`)

// codeSpans returns the byte spans of the record codes in text: joined tokens that contain a letter. A run of digits
// joined by "-" alone ("2-8") is a range, not a code, and stays checked.
func codeSpans(text string) [][2]int {
	var spans [][2]int
	for _, loc := range codeToken.FindAllStringIndex(text, -1) {
		if strings.IndexFunc(text[loc[0]:loc[1]], func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0 {
			spans = append(spans, [2]int{loc[0], loc[1]})
		}
	}
	return spans
}

// inSpans reports whether the byte offset lies inside one of the spans.
func inSpans(spans [][2]int, at int) bool {
	for _, sp := range spans {
		if at >= sp[0] && at < sp[1] {
			return true
		}
	}
	return false
}

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
	codes := codeSpans(reply)
	for _, loc := range numberToken.FindAllStringIndex(reply, -1) {
		if inSpans(codes, loc[0]) {
			continue
		}
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
