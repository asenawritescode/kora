package ai

import (
	"strings"
	"testing"
)

func TestPreviewsText(t *testing.T) {
	tests := []struct {
		name  string
		cards []previewCard
		want  string
	}{
		{
			name:  "one write asks once",
			cards: []previewCard{{WriteID: "w1", Summary: "create Item (item_name Sugar 1kg)"}},
			want:  "Ready to save: create Item (item_name Sugar 1kg). Save this?",
		},
		{
			name: "several writes share one card and one question",
			cards: []previewCard{
				{WriteID: "w1", Summary: "create Item (item_name Soda)"},
				{WriteID: "w2", Summary: "create Item (item_name Fanta)"},
			},
			want: "Ready to save 2 writes:\n- create Item (item_name Soda)\n- create Item (item_name Fanta)\nSave all?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := previewsText(tc.cards); got != tc.want {
				t.Fatalf("previewsText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPreviewsTextNeverShowsWriteID(t *testing.T) {
	got := previewsText([]previewCard{{WriteID: "01HZSECRETWRITE", Summary: "update Item ITM-1"}})
	if strings.Contains(got, "01HZSECRETWRITE") {
		t.Fatalf("reply shows the write_id: %q", got)
	}
}
