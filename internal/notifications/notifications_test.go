package notifications

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type testClock struct{ now time.Time }

func (c testClock) Now() time.Time { return c.now }

type testIDs struct{}

func (testIDs) NewID() string { return contract.NewID() }

type testPlanner struct {
	items []PlannedDelivery
	err   error
	calls int
}

func (p *testPlanner) Plan(context.Context, contract.EventEnvelope) ([]PlannedDelivery, error) {
	p.calls++
	return p.items, p.err
}

type testStore struct {
	recorded   bool
	rows       []Delivery
	claimed    []Delivery
	claimCalls int
	err        error
	acceptErr  error
	accepted   []NotificationID
	retried    []struct {
		id        NotificationID
		exhausted bool
	}
}

func (s *testStore) HasEvent(context.Context, contract.TenantID, string) (bool, error) {
	return s.recorded, nil
}
func (s *testStore) RecordEvent(_ context.Context, _ contract.TenantID, _ string, rows []Delivery, _ time.Time) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.recorded = true
	s.rows = append(s.rows, rows...)
	return true, nil
}
func (s *testStore) Claim(context.Context, contract.TenantID, string, time.Time, time.Time, int) ([]Delivery, error) {
	s.claimCalls++
	return s.claimed, s.err
}
func (s *testStore) Accept(_ context.Context, _ contract.TenantID, id NotificationID, _ string, _ int64, _ DeliveryResult, _ time.Time) error {
	s.accepted = append(s.accepted, id)
	return s.acceptErr
}
func (s *testStore) Retry(_ context.Context, _ contract.TenantID, id NotificationID, _ string, _ int64, _ time.Time, _ string, exhausted bool, _ time.Time) error {
	s.retried = append(s.retried, struct {
		id        NotificationID
		exhausted bool
	}{id, exhausted})
	return s.err
}

type testSender struct {
	err   error
	sends int
}

type notificationAdmission struct {
	calls    atomic.Int32
	releases atomic.Int32
	failAt   int32
}

func (a *notificationAdmission) Acquire(context.Context) (func(), error) {
	if a.calls.Add(1) == a.failAt && a.failAt != 0 {
		return nil, errors.New("work admission exhausted")
	}
	return func() { a.releases.Add(1) }, nil
}

func (s *testSender) Send(context.Context, Delivery) (DeliveryResult, error) {
	s.sends++
	return DeliveryResult{}, s.err
}

func testEvent() contract.EventEnvelope {
	return contract.EventEnvelope{ID: "event-1", Type: "request.created", Version: 1, Source: "engine", Site: "tenant-a",
		AggregateType: "request", AggregateID: "request-1", OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Data: []byte(`{}`)}
}

func TestConsumePersistsPlannedDeliveriesAndSkipsRecordedEvent(t *testing.T) {
	clock := testClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := &testStore{}
	planner := &testPlanner{items: []PlannedDelivery{{Recipient: "user-1", RequestID: "request-1", Channel: ChannelEmail}}}
	service, err := NewService("tenant-a", store, planner, clock, testIDs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Consume(context.Background(), testEvent()); err != nil {
		t.Fatal(err)
	}
	if len(store.rows) != 1 || store.rows[0].Status != DeliveryPending || store.rows[0].Tenant != "tenant-a" || store.rows[0].EventID != "event-1" {
		t.Fatalf("stored delivery = %#v", store.rows)
	}
	if err := service.Consume(context.Background(), testEvent()); err != nil {
		t.Fatal(err)
	}
	if planner.calls != 1 || len(store.rows) != 1 {
		t.Fatalf("duplicate event replanned or duplicated rows: planner calls=%d rows=%d", planner.calls, len(store.rows))
	}
}

func TestConsumeRejectsDuplicateAndMismatchedPlan(t *testing.T) {
	clock := testClock{time.Now()}
	for _, tc := range []struct {
		name  string
		items []PlannedDelivery
	}{
		{name: "duplicate", items: []PlannedDelivery{{Recipient: "u1", RequestID: "request-1", Channel: ChannelInbox}, {Recipient: "u1", RequestID: "request-1", Channel: ChannelInbox}}},
		{name: "wrong request", items: []PlannedDelivery{{Recipient: "u1", RequestID: "request-2", Channel: ChannelInbox}}},
		{name: "unsupported channel", items: []PlannedDelivery{{Recipient: "u1", RequestID: "request-1", Channel: "sms"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &testStore{}
			service, err := NewService("tenant-a", store, &testPlanner{items: tc.items}, clock, testIDs{})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Consume(context.Background(), testEvent()); err == nil {
				t.Fatal("Consume succeeded with invalid plan")
			}
			if len(store.rows) != 0 {
				t.Fatalf("invalid plan persisted %d deliveries", len(store.rows))
			}
		})
	}
}

func TestWorkerRecordsRetryAndTerminalFailure(t *testing.T) {
	clock := testClock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := &testStore{claimed: []Delivery{{ID: "delivery-1", Attempt: 1, ClaimToken: 3}, {ID: "delivery-2", Attempt: 2, ClaimToken: 4}}}
	sender := &testSender{err: errors.New("temporary")}
	worker, err := NewWorker("tenant-a", store, sender, clock, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Configure(time.Second, 2, 2, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	accepted, err := worker.RunOnce(context.Background())
	if err != nil || accepted != 0 {
		t.Fatalf("RunOnce = (%d, %v)", accepted, err)
	}
	if len(store.retried) != 2 || store.retried[0].exhausted || !store.retried[1].exhausted {
		t.Fatalf("retry state = %#v", store.retried)
	}
	if err := worker.Configure(MaxLeaseDuration+time.Second, 2, 2, time.Second); err == nil {
		t.Fatal("worker accepted lease above the configured bound")
	}
}

func TestWorkerPropagatesAcknowledgmentFailure(t *testing.T) {
	clock := testClock{time.Now()}
	store := &testStore{claimed: []Delivery{{ID: "delivery-1", Attempt: 1, ClaimToken: 1}}, acceptErr: errors.New("ack failed")}
	sender := &testSender{}
	worker, err := NewWorker("tenant-a", store, sender, clock, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce swallowed acknowledgment failure")
	}
}

func TestWorkerAdmitsEachDatabaseUnitButNotSenderCall(t *testing.T) {
	clock := testClock{time.Now()}
	store := &testStore{claimed: []Delivery{{ID: "delivery-1", Attempt: 1, ClaimToken: 1}}}
	sender := &testSender{}
	admission := &notificationAdmission{}
	worker, err := NewWorkerWithAdmission("tenant-a", store, sender, clock, "worker-1", admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal("run admitted notification:", err)
	}
	if admission.calls.Load() != 2 || admission.releases.Load() != 2 {
		t.Fatalf("admission calls/releases = %d/%d, want claim and acknowledgment only", admission.calls.Load(), admission.releases.Load())
	}
	if sender.sends != 1 || store.claimCalls != 1 || len(store.accepted) != 1 {
		t.Fatalf("sender/store calls = %d/%d/%d, want one each", sender.sends, store.claimCalls, len(store.accepted))
	}
}

func TestWorkerDoesNotClaimWithoutAdmission(t *testing.T) {
	store := &testStore{claimed: []Delivery{{ID: "delivery-1", Attempt: 1, ClaimToken: 1}}}
	admission := &notificationAdmission{failAt: 1}
	worker, err := NewWorkerWithAdmission("tenant-a", store, &testSender{}, testClock{time.Now()}, "worker-1", admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce succeeded without claim admission")
	}
	if store.claimCalls != 0 || admission.releases.Load() != 0 {
		t.Fatalf("claim calls/releases = %d/%d, want none", store.claimCalls, admission.releases.Load())
	}
}
