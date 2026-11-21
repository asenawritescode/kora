package ai

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestUnmatchedNumbersAgainstFigures(t *testing.T) {
	cases := []struct {
		name    string
		message string
		figures []float64
		reply   string
		want    []string
	}{
		{"figures as printed", "", []float64{112480, 8.4}, "Sales were KES 112,480, up 8.4%.", nil},
		{"whole units of a result with cents", "", []float64{16445.4}, "KES 16,445 was credited.", nil},
		{"a figure the result lacks", "", []float64{16445}, "KES 16,446 was credited.", []string{"16,446"}},
		{"one decimal more than the figure", "", []float64{8.4}, "up 8.45%", []string{"8.45"}},
		{"an estimate is still wrong", "", []float64{112480}, "about 112,000 sales", []string{"112,000"}},
		{"the person's own number is allowed", "top 5 products", nil, "Here are the top 5.", nil},
		{"the card's clock and dates are allowed", "Data as of 19:14 · days end 04:00 · 9 Nov – 15 Nov 2026", nil, "Data as of 19:14, days end 04:00, 9 Nov 2026.", nil},
		{"no figures in the reply", "", []float64{1}, "Here are the sales.", nil},
		{"thousands suffix is checked scaled", "", []float64{12000}, "KES 12k in credit.", nil},
		{"thousands suffix off by one", "", []float64{12500}, "KES 12k in credit.", []string{"12k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var allowed numberSet
			allowed.addText(tc.message)
			for _, f := range tc.figures {
				allowed.addFigure(f)
			}
			got := unmatchedNumbers(tc.reply, allowed)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("unmatched = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrintAmountFollowsFormat(t *testing.T) {
	cases := []struct {
		raw, format, currency, want string
	}{
		{"16445.4", "currency", "KES", "KES 16,445"},
		{"112480", "currency", "KES", "KES 112,480"},
		{"1234567", "number", "", "1,234,567"},
		{"8.44", "percent", "", "8.4%"},
		{"-1500", "currency", "KES", "KES -1,500"},
	}
	for _, tc := range cases {
		v, ok := valueOf([]byte(`"` + tc.raw + `"`))
		if !ok {
			t.Fatalf("%s is not a value", tc.raw)
		}
		if got := printAmount(v, true, tc.format, tc.currency); got != tc.want {
			t.Errorf("printAmount(%s, %s) = %q, want %q", tc.raw, tc.format, got, tc.want)
		}
	}
	if got := printAmount(decimal.Zero, false, "currency", "KES"); got != "—" {
		t.Errorf("missing value prints %q, want a dash", got)
	}
}
