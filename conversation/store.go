package conversation

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrNotFound = errors.New("conversation not found")

type Store struct {
	DB     *sql.DB
	Rebind func(string) string
}

func (s Store) bind(q string) string {
	if s.Rebind != nil {
		return s.Rebind(q)
	}
	return q
}

func ID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return prefix + "-" + fmt.Sprint(time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b)
}

func (s Store) Create(tenant, channel, contact, correlation string) (Conversation, error) {
	now := time.Now().UTC()
	id := ID("conv")
	if tenant == "" {
		return Conversation{}, errors.New("tenant is required")
	}
	if channel == "" {
		channel = "web"
	}
	if correlation == "" {
		correlation = ID("cor")
	}
	_, err := s.DB.Exec(s.bind(`INSERT INTO _kora_conversation (id, tenant_id, channel, external_contact_id, current_topic, status, correlation_id, last_message_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`), id, tenant, channel, contact, TopicBusiness, "active", correlation, now, now)
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{ID: id, TenantID: tenant, Channel: channel, ExternalContactID: contact, CurrentTopic: TopicBusiness, Status: "active", CorrelationID: correlation, LastMessageAt: now, CreatedAt: now}, nil
}

func (s Store) Get(tenant, id string) (Conversation, error) {
	var c Conversation
	err := s.DB.QueryRow(s.bind(`SELECT id, tenant_id, channel, external_contact_id, user_id, current_topic, status, correlation_id, last_message_at, created_at FROM _kora_conversation WHERE tenant_id = ? AND id = ?`), tenant, id).Scan(&c.ID, &c.TenantID, &c.Channel, &c.ExternalContactID, &c.UserID, &c.CurrentTopic, &c.Status, &c.CorrelationID, &c.LastMessageAt, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	return c, err
}

func (s Store) Messages(tenant, id string) ([]Message, error) {
	if _, err := s.Get(tenant, id); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(s.bind(`SELECT m.id, m.conversation_id, m.actor, m.text, m.question_key, m.interpretation, m.created_at FROM _kora_conversation_message m JOIN _kora_conversation c ON c.id=m.conversation_id WHERE c.tenant_id=? AND m.conversation_id=? ORDER BY m.created_at, m.id`), tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Actor, &m.Text, &m.QuestionKey, &m.Interpretation, &m.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

func (s Store) AddMessage(tenant, id, actor, text, key, interpretation string) (Message, error) {
	if _, err := s.Get(tenant, id); err != nil {
		return Message{}, err
	}
	now := time.Now().UTC()
	m := Message{ID: ID("msg"), ConversationID: id, Actor: actor, Text: text, QuestionKey: key, Interpretation: interpretation, CreatedAt: now}
	_, err := s.DB.Exec(s.bind(`INSERT INTO _kora_conversation_message (id, conversation_id, actor, text, question_key, interpretation, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`), m.ID, id, actor, text, key, interpretation, now)
	if err == nil {
		_, err = s.DB.Exec(s.bind(`UPDATE _kora_conversation SET last_message_at=? WHERE tenant_id=? AND id=?`), now, tenant, id)
	}
	return m, err
}

func (s Store) SaveAnswer(tenant, id string, a Answer) error {
	if _, err := s.Get(tenant, id); err != nil {
		return err
	}
	raw, _ := json.Marshal(a.StructuredValue)
	_, err := s.DB.Exec(s.bind(`INSERT INTO _kora_interview_answer (id, conversation_id, question_key, original_text, structured_value, confidence, source_message_id, confirmation_state, feedback_shown, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), ID("ans"), id, a.QuestionKey, a.OriginalText, string(raw), a.Confidence, a.SourceMessageID, a.ConfirmationState, a.FeedbackShown, time.Now().UTC())
	return err
}

func NextTopic(topic Topic) Topic {
	switch topic {
	case TopicBusiness:
		return TopicGoal
	case TopicGoal:
		return TopicRecords
	case TopicRecords:
		return TopicProcess
	case TopicProcess:
		return TopicTeam
	case TopicTeam:
		return TopicReady
	default:
		return TopicReady
	}
}
func TopicLabel(t Topic) string { return strings.Title(string(t)) }
