package ai

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

// newAnswerTurn starts the answer state of one chat turn: what the person may see on a card, and the metrics
// runner. Only the administrator may pin a question or share it with the shop.
func newAnswerTurn(turn ChatTurn, currency string) *answerTurn {
	return &answerTurn{
		run:  turn.Metrics,
		page: cardPage{Currency: currency, CanShare: executionHasRole(turn.Execution, doctype.AdminRole)},
	}
}

// unbacked lists the numbers in a reply that the turn's figures do not back: not in the query or drill results,
// not on the cards, and not in the person's message. A turn with no metrics read has no figures to check against
// and returns nothing.
func (t *answerTurn) unbacked(reply, message string) []string {
	if len(t.queries)+len(t.drills)+len(t.building) == 0 {
		return nil
	}
	var allowed numberSet
	allowed.addText(message)
	for _, card := range t.cards() {
		allowed.addText(cardText(card))
	}
	for _, q := range t.queries {
		for _, row := range append(append([]contract.MetricRow{}, q.result.Rows...), q.result.Previous...) {
			addRowFigures(&allowed, row)
		}
	}
	for _, d := range t.drills {
		allowed.addText(d.result.Total)
		for _, rec := range d.result.Rows {
			allowed.addText(rec.Value)
		}
	}
	return unmatchedNumbers(reply, allowed)
}

// cardText is what a card prints, without its buttons: the figures a reply may repeat.
func cardText(card contract.AnswerCard) string {
	card.Actions = nil
	raw, err := json.Marshal(card)
	if err != nil {
		return ""
	}
	return string(raw)
}

// addRowFigures adds the figures of one result row, decimal strings or null.
func addRowFigures(s *numberSet, row contract.MetricRow) {
	for _, raw := range row.Values {
		if v, ok := valueOf(raw); ok {
			f, _ := v.Float64()
			s.addFigure(f)
		}
	}
}

// finalReply is the reply that may be sent. A reply that states a figure the turn does not back is replaced by a
// sentence with no number, which names the metric and its period; the cards stay.
func (t *answerTurn) finalReply(reply, message string) string {
	if len(t.unbacked(reply, message)) == 0 {
		return reply
	}
	return t.fallbackSentence()
}

// fallbackSentence states what the last figure was for, in words, without a number.
func (t *answerTurn) fallbackSentence() string {
	if len(t.queries) == 0 && len(t.building) > 0 {
		return "The history for " + strings.ToLower(t.building[len(t.building)-1].label) + " is still building, so I can't give that figure yet."
	}
	if len(t.queries) == 0 {
		return "I can't give an exact figure for that from this shop's data."
	}
	q := t.queries[len(t.queries)-1]
	cat, err := t.metricsCatalog()
	if err != nil {
		return "I can't give an exact figure for that from this shop's data."
	}
	period := strings.ReplaceAll(q.meta.Period.Preset, "_", " ")
	if q.meta.Period.Preset == "" || q.meta.Period.Preset == "custom" {
		period = "the period you asked for"
	}
	return "Here are " + strings.ToLower(labelOf(q, cat)) + " for " + period + "."
}

// completeAnswer stores a finished reply and closes its run, step and task. It is the one place a turn ends with a
// reply.
func completeAnswer(ctx context.Context, tx *orm.TxManager, a finalAnswer) {
	_ = AppendConversationMessage(ctx, tx.DB, MessageRecord{Site: a.Site, ConversationID: a.ConversationID, RunID: a.RunID, Role: "assistant", Content: a.Content, Kind: "summary", StepID: a.StepID})
	_ = UpdateStepStatus(ctx, tx.DB, a.StepID, StepUpdate{Status: "completed", Summary: a.Content, OutputJSON: a.Content})
	_ = MarkTaskStatus(ctx, tx.DB, a.StepID, "done", a.Content)
	_ = QueueFollowUpTasks(ctx, tx.DB, RunScope{Site: a.Site, RunID: a.RunID, ConversationID: a.ConversationID}, deriveFollowUpTasks(a.Content, toolResultsFromMessages(a.Messages)))
	_ = UpsertRun(ctx, tx.DB, RunRecord{
		ID:             a.RunID,
		Site:           a.Site,
		ConversationID: a.ConversationID,
		Channel:        "chat",
		Status:         "completed",
		InputMessage:   a.Message,
		OutputMessage:  a.Content,
		Summary:        a.Content,
		Model:          a.Model,
		Provider:       a.Provider,
	})
	_ = UpsertConversation(ctx, tx.DB, ConversationRecord{
		ID:            a.ConversationID,
		Site:          a.Site,
		Channel:       "chat",
		SubjectKey:    a.SubjectKey,
		Title:         truncateScript(a.Message, 240),
		Summary:       a.Content,
		Status:        "active",
		LastRunID:     a.RunID,
		LastMessageAt: time.Now().UTC(),
	})
	if len(a.Cards) > 0 {
		if raw, err := storeCards(a.Cards, a.Question); err == nil {
			_ = AppendConversationMessage(ctx, tx.DB, MessageRecord{Site: a.Site, ConversationID: a.ConversationID, RunID: a.RunID, Role: "assistant", Content: raw, Kind: answerKindCard, StepID: a.StepID})
		}
	}
	_ = SummarizeRun(ctx, tx.DB, a.RunID)
}

// finalAnswer is a reply that ends a turn, with the run it belongs to.
type finalAnswer struct {
	Site, ConversationID, RunID, StepID, Message, SubjectKey, Model, Provider, Content string
	Messages                                                                           []map[string]any
	Cards                                                                              []contract.AnswerCard
	Question                                                                           json.RawMessage
}

// completeChoice ends a turn with a clarifying question. The person's pick is the next turn.
func completeChoice(ctx context.Context, tx *orm.TxManager, a finalAnswer, choice contract.ChoiceQuestion) {
	raw, err := json.Marshal(choice)
	if err == nil {
		_ = AppendConversationMessage(ctx, tx.DB, MessageRecord{Site: a.Site, ConversationID: a.ConversationID, RunID: a.RunID, Role: "assistant", Content: string(raw), Kind: answerKindChoices, StepID: a.StepID})
	}
	_ = AppendConversationMessage(ctx, tx.DB, MessageRecord{Site: a.Site, ConversationID: a.ConversationID, RunID: a.RunID, Role: "assistant", Content: choice.Question, Kind: "summary", StepID: a.StepID})
	_ = UpdateStepStatus(ctx, tx.DB, a.StepID, StepUpdate{Status: "completed", Summary: choice.Question, OutputJSON: choice.Question})
	_ = MarkTaskStatus(ctx, tx.DB, a.StepID, "done", choice.Question)
	_ = UpsertRun(ctx, tx.DB, RunRecord{ID: a.RunID, Site: a.Site, ConversationID: a.ConversationID, Channel: "chat", Status: "completed", InputMessage: a.Message, OutputMessage: choice.Question, Summary: choice.Question, Model: a.Model, Provider: a.Provider})
	_ = SummarizeRun(ctx, tx.DB, a.RunID)
}

// endWithChoice stores a clarifying question as the end of its turn and returns the response that carries it.
func endWithChoice(ctx context.Context, tx *orm.TxManager, choice *contract.ChoiceQuestion, a finalAnswer) ChatResponse {
	completeChoice(ctx, tx, a, *choice)
	return ChatResponse{Reply: choice.Question, RunID: a.RunID, Choices: choice}
}

// correction returns the messages that send a reply with a wrong number back to the model, once. It returns nil
// when the reply may stand, or when the one correction was already sent.
func (t *answerTurn) correction(reply, message string) []map[string]any {
	bad := t.unbacked(reply, message)
	if len(bad) == 0 || t.corrected {
		return nil
	}
	t.corrected = true
	return []map[string]any{
		{"role": "assistant", "content": reply},
		{"role": "user", "content": correctionFor(bad)},
	}
}
