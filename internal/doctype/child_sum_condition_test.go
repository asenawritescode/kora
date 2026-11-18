package doctype

import "testing"

// childSumDoc returns a Till-style document with two count lines totalling 350.
func childSumDoc(counted float64) *Document {
	doc := NewDocument("Till Session")
	doc.Set("closing_count", counted)
	for _, amount := range []float64{300, 50} {
		line := NewDocument("Till Count Line")
		line.Set("amount", amount)
		doc.Set("closing_counts", append(doc.GetTable("closing_counts"), line))
	}
	return doc
}

func TestConditionSumsChildRows(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		counted   float64
		want      bool
	}{
		{"s-expression sum matches", `(== closing_count (sum "closing_counts" "amount"))`, 350, true},
		{"s-expression sum mismatches", `(== closing_count (sum "closing_counts" "amount"))`, 999, false},
		{"infix sum matches", `SUM(closing_counts.amount) = closing_count`, 350, true},
		{"infix sum mismatches", `SUM(closing_counts.amount) = closing_count`, 999, false},
		{"infix sum with double equals mismatches", `SUM(closing_counts.amount) == closing_count`, 999, false},
		{"infix sum not equal", `SUM(closing_counts.amount) != closing_count`, 350, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := evaluateCondition(tc.condition, childSumDoc(tc.counted), nil); got != tc.want {
				t.Fatalf("evaluateCondition(%q) = %v, want %v", tc.condition, got, tc.want)
			}
		})
	}
}

func TestInfixConditionRejectsSingleEquals(t *testing.T) {
	for _, condition := range []string{`status = 'Closed'`, `closing_count = 999`} {
		if err := infixCondition(condition).validateForm(); err == nil {
			t.Fatalf("validateForm(%q) accepted single =", condition)
		}
	}
	if err := infixCondition(`status == 'Closed'`).validateForm(); err != nil {
		t.Fatalf("== rejected: %v", err)
	}
}

func TestInfixSumWithoutComparisonIsRejected(t *testing.T) {
	for _, condition := range []string{`SUM(closing_counts.amount) + 1`, `SUM(closing_counts.amount) > 0 AND closing_count > 0`} {
		if err := infixCondition(condition).validateForm(); err == nil {
			t.Fatalf("validateForm(%q) accepted an unsupported sum condition", condition)
		}
	}
}
