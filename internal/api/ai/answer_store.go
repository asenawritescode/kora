package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/asenawritescode/kora/contract"
)

// Kinds of the answer records a reply stores beside its text.
const (
	answerKindCard    = "card"
	answerKindChoices = "choices"
)

// AnswerAttachment is what a reply carried beside its text: its answer cards, or a clarifying question. The
// conversation endpoint returns them with the transcript so the workspace can draw them again.
type AnswerAttachment struct {
	RunID   string          `json:"run_id"`
	Kind    string          `json:"kind"`
	Content json.RawMessage `json:"content"`
}

// cardsRecord is the stored content of a card message: the cards of the reply and the question behind them, which
// the next turn of the conversation follows up on.
type cardsRecord struct {
	Cards    []contract.AnswerCard `json:"cards"`
	Question json.RawMessage       `json:"question,omitempty"`
}

// ListAnswerAttachments returns the cards and clarifying questions of a conversation, in order.
func ListAnswerAttachments(ctx context.Context, db *sql.DB, site, conversationID string) ([]AnswerAttachment, error) {
	rows, err := (aiSQL{db}).QueryContext(ctx, `
SELECT run_id, message_kind, content
FROM _kora_ai_message
WHERE site = ? AND conversation_id = ? AND message_kind IN ('card', 'choices')
ORDER BY sequence, created_at, id`, site, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AnswerAttachment{}
	for rows.Next() {
		var a AnswerAttachment
		var content string
		if err := rows.Scan(&a.RunID, &a.Kind, &content); err != nil {
			return nil, err
		}
		a.Content = json.RawMessage(content)
		out = append(out, a)
	}
	return out, rows.Err()
}

// lastAnswerQuestion is the question behind the latest card of a conversation, or nil when there is none. The next
// turn reuses it: "and the week before that?" keeps its metric and moves its period.
func lastAnswerQuestion(ctx context.Context, db *sql.DB, site, conversationID string) (json.RawMessage, error) {
	var content string
	err := (aiSQL{db}).QueryRowContext(ctx, `
SELECT content
FROM _kora_ai_message
WHERE site = ? AND conversation_id = ? AND message_kind = 'card'
ORDER BY sequence DESC
LIMIT 1`, site, conversationID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record cardsRecord
	if err := json.Unmarshal([]byte(content), &record); err != nil {
		return nil, err
	}
	return record.Question, nil
}

// storeCards is the content of the card message of a reply.
func storeCards(cards []contract.AnswerCard, question json.RawMessage) (string, error) {
	raw, err := json.Marshal(cardsRecord{Cards: cards, Question: question})
	return string(raw), err
}

// lastQuestion is the question of the latest query of the turn, the one a follow-up keeps.
func (t *answerTurn) lastQuestion() json.RawMessage {
	if len(t.queries) == 0 {
		return nil
	}
	return t.queries[len(t.queries)-1].question
}
