package db

import "testing"

func TestNowMinusSecondsExpression(t *testing.T) {
	tests := []struct {
		driver string
		want   string
	}{
		{"mysql", "DATE_SUB(CURRENT_TIMESTAMP(6), INTERVAL ? SECOND)"},
		{"postgres", "(NOW() - (? * INTERVAL '1 second'))"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			if got := Resolve(tc.driver).NowMinusSeconds(); got != tc.want {
				t.Fatalf("NowMinusSeconds() = %q, want %q", got, tc.want)
			}
		})
	}
}
