// Package notifications owns durable recipient-specific delivery attempts.
// It never treats delivery as the source of truth for a request or workflow.
package notifications

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type NotificationID string
type Channel string

const (
	ChannelInbox Channel = "inbox"
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"

	DeliveryPending   = "pending"
	DeliverySending   = "sending"
	DeliveryAccepted  = "accepted"
	DeliveryExhausted = "failed"

	MaxPlannedDeliveries = 500
	MaxDeliveryBatch     = 100
	MaxLeaseDuration     = 5 * time.Minute
	MaxErrorBytes        = 2048
)

// PlannedDelivery is a recipient/channel decision made by a composition-owned
// planner that can apply current request visibility and tenant membership.
type PlannedDelivery struct {
	Recipient contract.PrincipalID
	RequestID contract.RequestID
	Channel   Channel
}

// Delivery stores identifiers only. Sender implementations must re-resolve
// current recipient authorization before rendering; the notification queue
// does not contain request titles, comments, subject data, or email addresses.
type Delivery struct {
	ID                NotificationID
	Tenant            contract.TenantID
	Recipient         contract.PrincipalID
	RequestID         contract.RequestID
	Channel           Channel
	EventID           string
	Attempt           uint16
	Status            string
	ClaimOwner        string
	ClaimToken        int64
	LeaseUntil        *time.Time
	ProviderReference string
	AcceptedAt        *time.Time
	LastError         string
	AvailableAt       time.Time
	CreatedAt         time.Time
}

type DeliveryResult struct {
	ProviderReference string
	AcceptedAt        time.Time
}

type Sender interface {
	Send(ctx context.Context, delivery Delivery) (DeliveryResult, error)
}

type Planner interface {
	Plan(ctx context.Context, event contract.EventEnvelope) ([]PlannedDelivery, error)
}

type Clock interface{ Now() time.Time }
type IDSource interface{ NewID() string }

type Store interface {
	HasEvent(ctx context.Context, tenant contract.TenantID, eventID string) (bool, error)
	RecordEvent(ctx context.Context, tenant contract.TenantID, eventID string, deliveries []Delivery, at time.Time) (bool, error)
	Claim(ctx context.Context, tenant contract.TenantID, owner string, now, leaseUntil time.Time, limit int) ([]Delivery, error)
	Accept(ctx context.Context, tenant contract.TenantID, id NotificationID, owner string, token int64, result DeliveryResult, at time.Time) error
	Retry(ctx context.Context, tenant contract.TenantID, id NotificationID, owner string, token int64, next time.Time, cause string, exhausted bool, at time.Time) error
}

type Service struct {
	tenant  contract.TenantID
	store   Store
	planner Planner
	clock   Clock
	ids     IDSource
}

func NewService(tenant contract.TenantID, store Store, planner Planner, clock Clock, ids IDSource) (*Service, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("notification tenant: %w", err)
	}
	if store == nil || planner == nil || clock == nil || ids == nil {
		return nil, fmt.Errorf("notification service requires store, planner, clock and ID source")
	}
	return &Service{tenant: tenant, store: store, planner: planner, clock: clock, ids: ids}, nil
}

// Consume plans recipient deliveries and stores them atomically with the event
// receipt. A retry after a crash is harmless even if the outer outbox consumer
// receipt was not written yet.
func (s *Service) Consume(ctx context.Context, event contract.EventEnvelope) error {
	if ctx == nil {
		return fmt.Errorf("notification consume requires a context")
	}
	if err := event.Validate(); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "notification event is invalid")
	}
	if event.AggregateType != "request" || event.AggregateID == "" {
		return contract.NewError(contract.CodeValidationFailed, "notification event must identify a request")
	}
	requestID, err := contract.ParseRequestID(event.AggregateID)
	if err != nil {
		return contract.NewError(contract.CodeValidationFailed, "notification request ID is invalid")
	}
	// This read avoids re-resolving recipients on ordinary redeliveries. The
	// unique receipt insert remains the authority for concurrent consumers.
	recorded, err := s.store.HasEvent(ctx, s.tenant, event.ID)
	if err != nil {
		return fmt.Errorf("check notification event receipt: %w", err)
	}
	if recorded {
		return nil
	}
	planned, err := s.planner.Plan(ctx, event)
	if err != nil {
		return fmt.Errorf("plan notification recipients: %w", err)
	}
	if len(planned) > MaxPlannedDeliveries {
		return contract.NewError(contract.CodeValidationFailed, "notification recipient plan exceeds the batch limit")
	}
	now := s.clock.Now().UTC()
	deliveries := make([]Delivery, 0, len(planned))
	seen := make(map[PlannedDelivery]struct{}, len(planned))
	for _, item := range planned {
		if _, err := contract.ParsePrincipalID(string(item.Recipient)); err != nil {
			return contract.NewError(contract.CodeValidationFailed, "notification recipient is invalid")
		}
		if item.RequestID != requestID || !validChannel(item.Channel) {
			return contract.NewError(contract.CodeValidationFailed, "notification plan does not match its request event")
		}
		if _, ok := seen[item]; ok {
			return contract.NewError(contract.CodeValidationFailed, "notification recipient/channel is duplicated")
		}
		seen[item] = struct{}{}
		id := s.ids.NewID()
		if _, err := contract.ParseRequestID(id); err != nil {
			return contract.NewError(contract.CodeInternal, "notification ID source returned an invalid ID")
		}
		deliveries = append(deliveries, Delivery{ID: NotificationID(id), Tenant: s.tenant, Recipient: item.Recipient,
			RequestID: item.RequestID, Channel: item.Channel, EventID: event.ID, Status: DeliveryPending,
			AvailableAt: now, CreatedAt: now})
	}
	if _, err := s.store.RecordEvent(ctx, s.tenant, event.ID, deliveries, now); err != nil {
		return fmt.Errorf("persist notification event: %w", err)
	}
	return nil
}

func validChannel(channel Channel) bool {
	return channel == ChannelInbox || channel == ChannelEmail || channel == ChannelPush
}

type Worker struct {
	tenant        contract.TenantID
	store         Store
	sender        Sender
	clock         Clock
	owner         string
	lease         time.Duration
	batch         int
	maxAttempt    uint16
	idle          time.Duration
	workAdmission interface {
		Acquire(context.Context) (func(), error)
	}
}

func NewWorker(tenant contract.TenantID, store Store, sender Sender, clock Clock, owner string) (*Worker, error) {
	return newWorker(tenant, store, sender, clock, owner, nil)
}

// NewWorkerWithAdmission constructs a worker whose database units share the
// endpoint's active-work budget. Sender calls run outside database admission.
func NewWorkerWithAdmission(tenant contract.TenantID, store Store, sender Sender, clock Clock, owner string, admission interface {
	Acquire(context.Context) (func(), error)
}) (*Worker, error) {
	if admission == nil {
		return nil, fmt.Errorf("notification worker requires a work admission controller")
	}
	return newWorker(tenant, store, sender, clock, owner, admission)
}

func newWorker(tenant contract.TenantID, store Store, sender Sender, clock Clock, owner string, admission interface {
	Acquire(context.Context) (func(), error)
}) (*Worker, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("notification worker tenant: %w", err)
	}
	if store == nil || sender == nil || clock == nil || strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("notification worker requires store, sender, clock and owner")
	}
	worker := &Worker{tenant: tenant, store: store, sender: sender, clock: clock, owner: owner, lease: 30 * time.Second,
		batch: 25, maxAttempt: 8, idle: time.Second}
	worker.workAdmission = admission
	return worker, nil
}

// Configure bounds worker retention and delivery attempts. It rejects unsafe
// values rather than silently creating unbounded batches or leases.
func (w *Worker) Configure(lease time.Duration, batch int, maxAttempt uint16, idle time.Duration) error {
	if w == nil || lease <= 0 || lease > MaxLeaseDuration || batch < 1 || batch > MaxDeliveryBatch || maxAttempt < 1 || idle <= 0 {
		return contract.NewError(contract.CodeValidationFailed, "notification worker limits are invalid")
	}
	w.lease, w.batch, w.maxAttempt, w.idle = lease, batch, maxAttempt, idle
	return nil
}

func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("notification worker requires a context")
	}
	now := w.clock.Now().UTC()
	release, err := w.acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("admit notification claim: %w", err)
	}
	deliveries, err := w.store.Claim(ctx, w.tenant, w.owner, now, now.Add(w.lease), w.batch)
	release()
	if err != nil {
		return 0, fmt.Errorf("claim notification deliveries: %w", err)
	}
	accepted := 0
	var batchErr error
	for _, delivery := range deliveries {
		result, sendErr := w.sender.Send(ctx, delivery)
		at := w.clock.Now().UTC()
		if sendErr == nil {
			if result.AcceptedAt.IsZero() {
				result.AcceptedAt = at
			}
			if err := w.withAdmission(ctx, func() error {
				return w.store.Accept(ctx, w.tenant, delivery.ID, w.owner, delivery.ClaimToken, result, at)
			}); err != nil {
				batchErr = errors.Join(batchErr, fmt.Errorf("acknowledge notification %s: %w", delivery.ID, err))
				continue
			}
			accepted++
			continue
		}
		exhausted := delivery.Attempt >= w.maxAttempt
		next := at.Add(retryDelay(delivery.Attempt))
		if err := w.withAdmission(ctx, func() error {
			return w.store.Retry(ctx, w.tenant, delivery.ID, w.owner, delivery.ClaimToken, next, sendErr.Error(), exhausted, at)
		}); err != nil {
			batchErr = errors.Join(batchErr, fmt.Errorf("record notification failure %s: %w", delivery.ID, err))
		}
	}
	return accepted, batchErr
}

func (w *Worker) acquire(ctx context.Context) (func(), error) {
	if w.workAdmission == nil {
		return func() {}, nil
	}
	return w.workAdmission.Acquire(ctx)
}

func (w *Worker) withAdmission(ctx context.Context, work func() error) error {
	release, err := w.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return work()
}

func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("notification worker requires a context")
	}
	for {
		accepted, err := w.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if accepted > 0 {
			continue
		}
		timer := time.NewTimer(w.idle)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func retryDelay(attempt uint16) time.Duration {
	delay := time.Second
	for i := uint16(1); i < attempt && delay < time.Minute; i++ {
		delay *= 2
	}
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}
