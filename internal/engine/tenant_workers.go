package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/asenawritescode/kora/internal/eventstream"
	"log/slog"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/natsprovider"
	"github.com/asenawritescode/kora/internal/notifications"
	"github.com/asenawritescode/kora/internal/outbox"
)

// RunRealtimeBridge forwards tenant bus events to realtime listeners until the bus closes.
func RunRealtimeBridge(ctx context.Context, siteName string, bus eventstream.Bus, provider *natsprovider.Provider) {
	ch, err := bus.Subscribe()
	if err != nil {
		slog.Warn("realtime bridge subscribe failed", "site", siteName, "error", err)
		return
	}
	subjectPrefix := provider.Config().SubjectPrefix + ".realtime." + siteName + ".changes"
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(map[string]any{
				"id":          event.ID,
				"type":        "change",
				"transport":   "nats",
				"site":        event.Site,
				"resource":    "doctype:" + event.Doctype,
				"doctype":     event.Doctype,
				"doc_name":    event.DocName,
				"operation":   event.Operation,
				"occurred_at": event.Timestamp,
				"payload":     event.Data,
			})
			if err != nil {
				slog.Warn("realtime bridge marshal failed", "site", siteName, "error", err)
				continue
			}
			_ = provider.PublishSubject(ctx, subjectPrefix, payload, contract.NewEventID())
		}
	}
}

// RunNATSOutboxSideEffects publishes tenant outbox side effects to NATS.
func RunNATSOutboxSideEffects(ctx context.Context, siteName string, provider *natsprovider.Provider, bus eventstream.Bus) {
	cfg := provider.Config()
	cfg.ConsumerName = siteName + "-sideeffects"
	// The shared stream also carries browser realtime payloads, Cloud events,
	// tasks, and dead letters. Consuming the entire prefix feeds realtime
	// messages back into the analytics bus, which republishes them forever.
	// Outbox event types are canonically prefixed with "kora." before the
	// provider adds its own subject prefix, so subscribe only to that namespace.
	cfg.ConsumerSubject = OutboxSideEffectSubject(cfg.SubjectPrefix)
	cfg.MaxDeliver = 5
	consumer, err := natsprovider.NewConsumer(provider, cfg)
	if err != nil {
		slog.Warn("nats side-effect bridge init failed", "site", siteName, "error", err)
		return
	}

	handler := func(ctx context.Context, delivery contract.Delivery) error {
		event, err := DecodeOutboxDelivery(delivery)
		if err != nil {
			return err
		}
		// Every site has its own durable consumer on the shared stream. A
		// consumer must acknowledge events for other sites without publishing
		// them into its local bus.
		if event.Site != siteName {
			return nil
		}
		if bus != nil {
			return bus.Publish(event)
		}
		return nil
	}

	slog.Info("nats side-effect bridge started", "site", siteName, "consumer", cfg.ConsumerName)
	RunNATSConsumerWithBackoff(ctx, siteName, cfg.ConsumerName, func(runCtx context.Context) error {
		return consumer.Run(runCtx, handler)
	})
}

// RunNATSConsumerWithBackoff restarts a durable consumer after transport or
// settlement errors. The JetStream durable retains unsettled messages while
// the generation-owned worker waits; cancellation stops the retry timer and
// lets the runtime join this worker before closing its provider.
func RunNATSConsumerWithBackoff(ctx context.Context, siteName, consumerName string, run func(context.Context) error) {
	delay := 250 * time.Millisecond
	const maxDelay = 10 * time.Second
	for ctx.Err() == nil {
		err := run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("consumer returned without runtime cancellation")
		}
		slog.Warn("durable NATS consumer stopped; retrying", "site", siteName, "consumer", consumerName, "retry_in", delay, "error", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func OutboxSideEffectSubject(prefix string) string {
	return prefix + "." + prefix + ".>"
}

// DecodeOutboxDelivery decodes an outbox delivery into a ChangeEvent.
func DecodeOutboxDelivery(delivery contract.Delivery) (eventstream.ChangeEvent, error) {
	var envelope contract.EventEnvelope
	if err := json.Unmarshal(delivery.Data, &envelope); err != nil {
		return eventstream.ChangeEvent{}, err
	}
	if err := envelope.Validate(); err != nil {
		return eventstream.ChangeEvent{}, fmt.Errorf("invalid outbox event envelope: %w", err)
	}

	var payload struct {
		Data    map[string]any `json:"data"`
		OldData map[string]any `json:"old_data"`
	}
	if len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, &payload); err != nil {
			return eventstream.ChangeEvent{}, err
		}
	}

	return eventstream.ChangeEvent{
		ID:         envelope.ID,
		Sequence:   string(envelope.Sequence),
		Site:       envelope.Site,
		Doctype:    envelope.AggregateType,
		DocName:    envelope.AggregateID,
		Operation:  OutboxEventOperation(envelope.Type),
		Timestamp:  envelope.OccurredAt,
		ModifiedBy: envelope.Source,
		Data:       payload.Data,
		OldData:    payload.OldData,
	}, nil
}

// OutboxEventOperation maps an outbox event type to its EventOp.
func OutboxEventOperation(eventType string) eventstream.EventOp {
	switch {
	case strings.HasSuffix(eventType, ".after_insert"):
		return eventstream.EventInsert
	case strings.HasSuffix(eventType, ".after_delete"):
		return eventstream.EventDelete
	case strings.HasSuffix(eventType, ".after_submit"):
		return eventstream.EventSubmit
	case strings.HasSuffix(eventType, ".after_cancel"):
		return eventstream.EventCancel
	default:
		return eventstream.EventUpdate
	}
}

// RunOutboxPublisher repeatedly publishes bounded due batches from the durable
// tenant outbox through its configured destination.
func RunOutboxPublisher(ctx context.Context, p *outbox.Publisher, admission *dbaccess.BackgroundLane, backlog ...*outbox.BacklogMetrics) {
	if ctx == nil || p == nil {
		return
	}
	if admission != nil {
		p.WorkAdmission = admission
	}
	var snapshot *outbox.BacklogMetrics
	if len(backlog) > 0 {
		snapshot = backlog[0]
	}
	p.Failures = snapshot
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var refreshAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := p.PublishDue(publishCtx, 100)
			reportOutboxPublishFailure(snapshot, err)
			if snapshot != nil && (refreshAt.IsZero() || !time.Now().Before(refreshAt)) {
				if err := snapshot.Refresh(publishCtx, p); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("outbox publisher: backlog metrics refresh failed", "error", err)
				}
				refreshAt = time.Now().Add(30 * time.Second)
			}
			cancel()
		}
	}
}

// reportOutboxPublishFailure logs a failed pass; cancellation and busy skips are not failures.
func reportOutboxPublishFailure(snapshot *outbox.BacklogMetrics, err error) {
	if err == nil || errors.Is(err, context.Canceled) || dbaccess.IsBackgroundBusy(err) {
		return
	}
	recordOutboxPublishError(snapshot, err)
	slog.Warn("outbox publisher: publish due failed", "error", err)
}

func recordOutboxPublishError(snapshot *outbox.BacklogMetrics, err error) {
	if snapshot != nil && errors.Is(err, outbox.ErrLeaseLost) {
		snapshot.RecordLeaseLost()
	}
}

// RunWebhookOutboxDispatcher consumes durable document events and keeps the
// event-level receipt behind successful external delivery. Its batch and
// active-work admission are bounded; idle tenants retain no polling goroutine
// after their runtime generation is retired.
func RunWebhookOutboxDispatcher(ctx context.Context, dispatcher *outbox.Dispatcher, admission *dbaccess.BackgroundLane) {
	if ctx == nil || dispatcher == nil {
		return
	}
	// The dispatcher admits its SQL selection, receipt and local transaction
	// units individually. External webhook HTTP calls must not hold a database
	// work permit while waiting on extension servers.
	if admission != nil {
		dispatcher.WorkAdmission = admission
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := dispatcher.Run(workCtx, 25); err != nil && ctx.Err() == nil && !dbaccess.IsBackgroundBusy(err) {
			slog.Warn("webhook outbox: dispatch failed", "error", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunRequestNotificationDispatcher durably plans request notifications from
// tenant outbox rows. Failed planning leaves the event unreceipted for retry.
func RunRequestNotificationDispatcher(ctx context.Context, dispatcher *outbox.Dispatcher) {
	if ctx == nil || dispatcher == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		processed, err := dispatcher.Run(workCtx, 25)
		if err != nil && ctx.Err() == nil && !dbaccess.IsBackgroundBusy(err) {
			slog.Warn("request notification outbox: dispatch failed", "error_type", fmt.Sprintf("%T", err))
		}
		cancel()
		if processed > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ignoreBackgroundBusy treats a skipped background pass as an idle pass.
func ignoreBackgroundBusy(err error) error {
	if dbaccess.IsBackgroundBusy(err) {
		return nil
	}
	return err
}

// RunRequestNotificationWorker retries transient database and local-bus
// failures while the durable delivery row remains available for recovery.
func RunRequestNotificationWorker(ctx context.Context, worker *notifications.Worker) {
	if ctx == nil || worker == nil {
		return
	}
	delay := time.Second
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		accepted, err := worker.RunOnce(workCtx)
		cancel()
		err = ignoreBackgroundBusy(err)
		if err != nil && ctx.Err() == nil {
			slog.Warn("request notification delivery: attempt failed", "error_type", fmt.Sprintf("%T", err))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
			if delay < 30*time.Second {
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
			continue
		}
		delay = time.Second
		if accepted > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
