package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/orm"
)

// previewCard is one write the turn previewed and that waits for the person's yes. WriteID is for the next turn's
// confirm_write; the reply text never shows it.
type previewCard struct {
	WriteID string `json:"write_id"`
	Summary string `json:"summary"`
}

// previewsText is the reply that ends a turn with open previews: one card for all of them, and one question.
func previewsText(cards []previewCard) string {
	if len(cards) == 1 {
		return "Ready to save: " + cards[0].Summary + ". Save this?"
	}
	lines := make([]string, 0, len(cards))
	for _, card := range cards {
		lines = append(lines, "- "+card.Summary)
	}
	return fmt.Sprintf("Ready to save %d writes:\n%s\nSave all?", len(cards), strings.Join(lines, "\n"))
}

// endWithPreviews stores the preview card as the reply that ends the turn. No model round follows it.
func endWithPreviews(ctx context.Context, tx *orm.TxManager, cards []previewCard, a finalAnswer) ChatResponse {
	a.Content = previewsText(cards)
	completeAnswer(ctx, tx, a)
	return ChatResponse{Reply: a.Content, RunID: a.RunID, Previews: cards}
}
