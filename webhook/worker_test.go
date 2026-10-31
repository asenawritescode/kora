package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/eventstream"
)

type testEventBus struct {
	mu        sync.Mutex
	listeners []chan eventstream.ChangeEvent
}

type retryingWorkAdmission struct {
	attempts  int
	releases  int
	active    int
	remaining int
}

func (a *retryingWorkAdmission) Acquire(context.Context) (func(), error) {
	a.attempts++
	if a.remaining > 0 {
		a.remaining--
		return nil, errors.New("capacity exhausted")
	}
	a.active++
	return func() { a.releases++; a.active-- }, nil
}

func (b *testEventBus) AddListener(ch chan eventstream.ChangeEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listeners = append(b.listeners, ch)
}

func (b *testEventBus) RemoveListener(ch chan eventstream.ChangeEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, listener := range b.listeners {
		if listener == ch {
			b.listeners = append(b.listeners[:i], b.listeners[i+1:]...)
			return
		}
	}
}

func (b *testEventBus) Publish(event eventstream.ChangeEvent) {
	b.mu.Lock()
	listeners := append([]chan eventstream.ChangeEvent(nil), b.listeners...)
	b.mu.Unlock()
	for _, listener := range listeners {
		listener <- event
	}
}

func TestWorkerStopIsIdempotent(t *testing.T) {
	worker := NewWorker(nil, nil, "test")
	worker.Stop()
	worker.Stop()
}

func TestWorkerUsesBoundedMultiBusListenerQueue(t *testing.T) {
	inner := analytics.NewChannelBus(1, t.TempDir())
	bus, err := analytics.NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal("create multi bus:", err)
	}
	defer bus.Close()
	worker := NewWorker(nil, bus, "test")
	worker.StartContext(context.Background())
	if got := bus.ListenerCount(); got != 1 {
		t.Fatalf("registered listeners = %d, want worker registered", got)
	}
	if cap(worker.listenerCh) != defaultEventQueueSize || cap(worker.listenerCh) > analytics.MaxListenerQueueSize {
		t.Fatalf("webhook queue capacity = %d, want bounded %d", cap(worker.listenerCh), defaultEventQueueSize)
	}
	worker.Stop()
	if got := bus.ListenerCount(); got != 0 {
		t.Fatalf("listeners after stop = %d, want 0", got)
	}
}

func TestLiveNotificationEventDoesNotConsumeDatabaseAdmission(t *testing.T) {
	admission := &retryingWorkAdmission{remaining: 1}
	worker := NewWorker(nil, nil, "tenant")
	worker.WorkAdmission = admission
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := worker.processLiveEvent(ctx, eventstream.ChangeEvent{ID: "notification", Operation: eventstream.EventNotification})
	if err != nil {
		t.Fatalf("processLiveEvent: %v", err)
	}
	if admission.attempts != 0 || admission.releases != 0 {
		t.Fatalf("admission attempts/releases = %d/%d, want no DB admission for notification event", admission.attempts, admission.releases)
	}
}

func TestOutboxWebhookSubscriptionReadUsesDatabaseAdmission(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT name, endpoint_url, secret, subscriptions, timeout_sec, consecutive_failures`).
		WithArgs("tenant-a").
		WillReturnRows(sqlmock.NewRows([]string{"name", "endpoint_url", "secret", "subscriptions", "timeout_sec", "consecutive_failures"}))
	worker := NewWorker(database, nil, "tenant-a")
	admission := &retryingWorkAdmission{}
	worker.WorkAdmission = admission
	event := contract.EventEnvelope{
		ID: "evt-admitted", Type: "kora.invoice.after_save", Version: 1,
		Source: "kora.kernel", Site: "tenant-a", AggregateType: "Invoice", AggregateID: "INV-1",
		OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"data":{}}`),
	}
	if err := worker.ProcessOutboxEvent(context.Background(), event); err != nil {
		t.Fatalf("ProcessOutboxEvent: %v", err)
	}
	if admission.attempts != 1 || admission.releases != 1 {
		t.Fatalf("admission attempts/releases = %d/%d, want one admitted subscription query", admission.attempts, admission.releases)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("subscription query was not completed:", err)
	}
}

func TestProcessOutboxEventMapsDurableRecordPayload(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT name, endpoint_url, secret, subscriptions, timeout_sec, consecutive_failures`).
		WithArgs("tenant-a").
		WillReturnRows(sqlmock.NewRows([]string{"name", "endpoint_url", "secret", "subscriptions", "timeout_sec", "consecutive_failures"}))
	worker := NewWorker(database, nil, "tenant-a")
	event := contract.EventEnvelope{
		ID: "evt-stable", Type: "kora.invoice.after_save", Version: 1,
		Source: "kora.kernel", Site: "tenant-a", AggregateType: "Invoice", AggregateID: "INV-1",
		OccurredAt: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
		Data:       json.RawMessage(`{"data":{"total":12},"old_data":{"total":10}}`),
	}
	if err := worker.ProcessOutboxEvent(context.Background(), event); err != nil {
		t.Fatalf("ProcessOutboxEvent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("matching-extension query was not completed:", err)
	}
}

func TestProcessOutboxEventRejectsInvalidEnvelopeForRetry(t *testing.T) {
	worker := NewWorker(nil, nil, "tenant-a")
	if err := worker.ProcessOutboxEvent(context.Background(), contract.EventEnvelope{}); err == nil {
		t.Fatal("invalid outbox envelope was acknowledged")
	}
}

func TestOutboxReplaySkipsAnExtensionWithFinalizedDelivery(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT name, endpoint_url, secret, subscriptions, timeout_sec, consecutive_failures`).
		WithArgs("tenant-a").
		WillReturnRows(sqlmock.NewRows([]string{"name", "endpoint_url", "secret", "subscriptions", "timeout_sec", "consecutive_failures"}).
			AddRow("extension-a", "https://example.test", "secret", `[{"event":"kora.invoice.after_save"}]`, 10, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_webhook_delivery`).
		WithArgs("extension-a", "evt-stable").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	worker := NewWorker(database, nil, "tenant-a")
	event := contract.EventEnvelope{
		ID: "evt-stable", Type: "kora.invoice.after_save", Version: 1,
		Source: "kora.kernel", Site: "tenant-a", AggregateType: "Invoice", AggregateID: "INV-1",
		OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"data":{"total":12}}`),
	}
	if err := worker.ProcessOutboxEvent(context.Background(), event); err != nil {
		t.Fatalf("replay finalized event: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("replay attempted an already finalized external delivery:", err)
	}
}

func TestRetryDeadLettersContextClosesRowsAndUsesDialectQuery(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	mock.ExpectQuery(`SELECT id, extension_name, event_id, event_type, endpoint_url, attempt\s+FROM _kora_webhook_delivery\s+WHERE status = 'dead_lettered' AND attempt < \$1\s+ORDER BY created_at LIMIT 100`).
		WithArgs(len(DefaultRetrySchedule())).WillReturnRows(sqlmock.NewRows([]string{"id", "extension_name", "event_id", "event_type", "endpoint_url", "attempt"}).
		AddRow("delivery-1", "ext", "event-1", "created", "https://example.test", 1).
		AddRow("delivery-done", "ext", "event-2", "created", "https://example.test", len(DefaultRetrySchedule())-1))
	mock.ExpectExec(`UPDATE _kora_webhook_delivery SET status = 'retrying', attempt = \$1, next_retry_at = \$2\s+WHERE id = \$3 AND status = 'dead_lettered' AND attempt = \$4`).
		WithArgs(2, sqlmock.AnyArg(), "delivery-1", 1).WillReturnResult(sqlmock.NewResult(0, 1))
	worker := NewWorker(database, nil, "tenant", db.Resolve("postgres"))
	if err := worker.RetryDeadLettersContext(context.Background()); err != nil {
		t.Fatalf("retry dead letters: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryDeadLettersAdmitsEachSQLPhase(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT id, extension_name, event_id, event_type, endpoint_url, attempt\s+FROM _kora_webhook_delivery`).
		WithArgs(len(DefaultRetrySchedule())).WillReturnRows(sqlmock.NewRows([]string{"id", "extension_name", "event_id", "event_type", "endpoint_url", "attempt"}).
		AddRow("delivery-1", "ext", "event-1", "created", "https://example.test", 1))
	mock.ExpectExec(`UPDATE _kora_webhook_delivery SET status = 'retrying'`).
		WithArgs(2, sqlmock.AnyArg(), "delivery-1", 1).WillReturnResult(sqlmock.NewResult(0, 1))
	admission := &retryingWorkAdmission{}
	worker := NewWorker(database, nil, "tenant", db.Resolve("mysql"))
	worker.WorkAdmission = admission
	if err := worker.RetryDeadLettersContext(context.Background()); err != nil {
		t.Fatal("retry dead letters:", err)
	}
	if admission.attempts != 2 || admission.releases != 2 || admission.active != 0 {
		t.Fatalf("admission attempts/releases/active = %d/%d/%d, want 2/2/0 for selection and update", admission.attempts, admission.releases, admission.active)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryDeadLettersContextPropagatesCancellation(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	worker := NewWorker(database, nil, "tenant", db.Resolve("mysql"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.RetryDeadLettersContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry error = %v, want context.Canceled", err)
	}
}

func TestWorkerStopCancelsAndJoinsInFlightDelivery(t *testing.T) {
	requestStarted := make(chan struct{})
	requestStopped := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHandler) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseHandler
		close(requestStopped)
	}))
	defer server.Close()
	defer release()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create sql mock:", err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT name, endpoint_url, secret, subscriptions, timeout_sec, consecutive_failures").
		WithArgs("test").
		WillReturnRows(sqlmock.NewRows([]string{"name", "endpoint_url", "secret", "subscriptions", "timeout_sec", "consecutive_failures"}).
			AddRow("extension", server.URL, "secret", `[{"event":"kora.invoice.after_insert"}]`, 10, 0))
	mock.ExpectExec("INSERT INTO _kora_webhook_delivery").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE _kora_extension SET consecutive_failures = consecutive_failures \+ 1`).WillReturnResult(sqlmock.NewResult(0, 1))

	bus := &testEventBus{}
	worker := NewWorker(db, bus, "test")
	worker.StartContext(context.Background())
	bus.Publish(eventstream.ChangeEvent{Site: "test", Doctype: "Invoice", Operation: eventstream.EventInsert, Timestamp: time.Now()})
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("worker did not begin webhook request")
	}

	worker.Stop()
	select {
	case <-worker.done:
	default:
		t.Fatal("Stop returned before the worker goroutine exited")
	}
	release()
	select {
	case <-requestStopped:
	case <-time.After(time.Second):
		t.Fatal("webhook server handler did not exit after test release")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("worker did not finish its delivery bookkeeping: %v", err)
	}
}
