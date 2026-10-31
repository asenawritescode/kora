package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/outbox"
)

const outboxConsumerName = "engine-webhooks-v1"

// NewOutboxDispatcher registers the webhook worker for canonical document
// lifecycle events. The outbox receipt is written only after every matching
// extension accepted the delivery or produced a permanent 4xx result.
func NewOutboxDispatcher(database *sql.DB, dialect db.QueryDialect, worker *Worker) (*outbox.Dispatcher, error) {
	if database == nil || dialect == nil || worker == nil {
		return nil, fmt.Errorf("webhook outbox dispatcher requires database, dialect and worker")
	}
	dispatcher := outbox.NewDispatcher(database, dialect)
	if err := dispatcher.RegisterForPatterns(outboxConsumerName, []string{
		"kora.%.after!_insert", "kora.%.after!_save", "kora.%.after!_delete",
		"kora.%.after!_submit", "kora.%.after!_cancel",
	}, worker.ProcessOutboxEvent); err != nil {
		return nil, err
	}
	return dispatcher, nil
}

// ProcessOutboxEvent delivers one durable document event. The dispatcher
// records its event-level receipt after success; a failed external delivery
// leaves the event retryable. Idempotency-Key is stable per event and extension.
func (w *Worker) ProcessOutboxEvent(ctx context.Context, envelope contract.EventEnvelope) error {
	if ctx == nil {
		return fmt.Errorf("webhook outbox delivery requires a context")
	}
	if err := envelope.Validate(); err != nil {
		return fmt.Errorf("validate webhook source event: %w", err)
	}
	operation, ok := eventOperation(envelope.Type)
	if !ok {
		return nil
	}
	var data struct {
		Data    map[string]any `json:"data"`
		OldData map[string]any `json:"old_data"`
	}
	if len(envelope.Data) != 0 {
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			return fmt.Errorf("decode webhook source event data: %w", err)
		}
	}
	event := eventstream.ChangeEvent{
		ID: envelope.ID, Site: envelope.Site, Doctype: envelope.AggregateType,
		DocName: envelope.AggregateID, Operation: operation,
		Timestamp: envelope.OccurredAt, ModifiedBy: envelope.Actor.PrincipalID,
		Data: data.Data, OldData: data.OldData,
	}
	return w.processEventWithReceiptCheck(ctx, event, true)
}

func eventOperation(eventType string) (eventstream.EventOp, bool) {
	for _, candidate := range []struct {
		suffix string
		op     eventstream.EventOp
	}{
		{".after_insert", eventstream.EventInsert},
		{".after_save", eventstream.EventUpdate},
		{".after_delete", eventstream.EventDelete},
		{".after_submit", eventstream.EventSubmit},
		{".after_cancel", eventstream.EventCancel},
	} {
		if strings.HasPrefix(eventType, "kora.") && strings.HasSuffix(eventType, candidate.suffix) {
			return candidate.op, true
		}
	}
	return "", false
}
