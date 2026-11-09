package outbox

import "testing"

func TestClassifyEventType(t *testing.T) {
	cases := []struct {
		eventType string
		want      EventCategory
	}{
		{"kora.sales_invoice.after_insert", EventCategoryTechnical},
		{"invoice.submitted", EventCategoryDomain},
		{"payment.received", EventCategoryDomain},
		{"deployment.completed", EventCategoryOperational},
		{"component.failed", EventCategoryOperational},
		{"something.else", EventCategoryUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyEventType(tc.eventType); got != tc.want {
			t.Fatalf("ClassifyEventType(%q) = %q, want %q", tc.eventType, got, tc.want)
		}
	}
}
