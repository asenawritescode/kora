package metrics

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestParseFormulaRejects(t *testing.T) {
	tests := []struct {
		name, source, wantErr string
	}{
		{"ends early", "1 +", "ends early"},
		{"text outside the grammar", "a ** 2", "unexpected \"*\""},
		{"unsupported character", "a ^ 2", "unsupported text"},
		{"malformed number", "1.2.3", "unsupported text"},
		{"missing closing parenthesis", "(a + 1", "closing parenthesis"},
		{"two operands in a row", "a b", "unexpected \"b\""},
		{"stray closing parenthesis", "a )", "unexpected \")\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFormula(tc.source)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v; want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestFormulaEval(t *testing.T) {
	tests := []struct {
		name   string
		source string
		vals   []decimal.NullDecimal // slot 0 is a, slot 1 is b
		want   string                // empty means null
	}{
		{"precedence", "1 + 2 * 3", nil, "7"},
		{"parentheses", "(1 + 2) * 3", nil, "9"},
		{"subtraction is left associative", "10 - 4 - 3", nil, "3"},
		{"division is left associative", "12 / 4 / 3", nil, "1"},
		{"decimal literal", "1.5 * 2", nil, "3"},
		{"metric references", "a - b", vals(v("100"), v("30")), "70"},
		{"division by zero is null", "a / b", vals(v("100"), v("0")), ""},
		{"null operand is null", "a + b", vals(v("100"), null), ""},
		{"derived zero divisor is null", "a * 2 / (b - b)", vals(v("1"), v("5")), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, err := parseFormula(tc.source)
			if err != nil {
				t.Fatalf("parseFormula: %v", err)
			}
			root.bind(map[string]int{"a": 0, "b": 1})
			got := root.eval(tc.vals)
			if tc.want == "" {
				if got.Valid {
					t.Fatalf("eval = %s, want null", got.Decimal)
				}
				return
			}
			if !got.Valid || !got.Decimal.Equal(dec(tc.want)) {
				t.Fatalf("eval = %+v, want %s", got, tc.want)
			}
		})
	}
}

var null = decimal.NullDecimal{}

func v(s string) decimal.NullDecimal { return decimal.NullDecimal{Decimal: dec(s), Valid: true} }

func vals(xs ...decimal.NullDecimal) []decimal.NullDecimal { return xs }
