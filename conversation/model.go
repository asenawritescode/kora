package conversation

import "time"

// Topic is deliberately expressed in business language. These keys are also
// stable API values, so clients can render progress without knowing schema
// internals.
type Topic string

const (
	TopicBusiness Topic = "business"
	TopicGoal     Topic = "goal"
	TopicRecords  Topic = "records"
	TopicProcess  Topic = "process"
	TopicTeam     Topic = "team"
	TopicReady    Topic = "ready"
)

type Conversation struct {
	ID                string    `json:"id"`
	TenantID          string    `json:"tenant_id"`
	Channel           string    `json:"channel"`
	ExternalContactID string    `json:"external_contact_id,omitempty"`
	UserID            string    `json:"user_id,omitempty"`
	CurrentTopic      Topic     `json:"current_topic"`
	Status            string    `json:"status"`
	CorrelationID     string    `json:"correlation_id"`
	LastMessageAt     time.Time `json:"last_message_at"`
	CreatedAt         time.Time `json:"created_at"`
}

type Message struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Actor          string    `json:"actor"`
	Text           string    `json:"text"`
	QuestionKey    string    `json:"question_key,omitempty"`
	Interpretation string    `json:"interpretation,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type Answer struct {
	QuestionKey       string         `json:"question_key"`
	OriginalText      string         `json:"original_text"`
	StructuredValue   map[string]any `json:"structured_value,omitempty"`
	Confidence        float64        `json:"confidence"`
	SourceMessageID   string         `json:"source_message_id"`
	ConfirmationState string         `json:"confirmation_state"`
	FeedbackShown     string         `json:"feedback_shown,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
}

type Progress struct {
	Current   Topic   `json:"current_topic"`
	Completed []Topic `json:"completed"`
}
