//go:build integration

package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestMySQLTransactionalConsumerReceiptAndLocalEffect(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open mysql:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_outbox_tx_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = root.Close()
	})
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse mysql DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	defer database.Close()
	dialect := db.Resolve("mysql")
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create outbox schema: %v\n%s", err, statement)
		}
	}
	if _, err := database.Exec(`CREATE TABLE local_effect (event_id VARCHAR(64) PRIMARY KEY) ENGINE=InnoDB`); err != nil {
		t.Fatal("create effect table:", err)
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "request.created", Version: 1, Source: "engine", Site: "tenant-a",
		Sequence: "41", AggregateType: "request", AggregateID: contract.NewID(), OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{}`)}
	createdAt := time.Now().UTC().Add(-2 * time.Minute)
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal("marshal event:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_outbox (id, site, event_type, event_version, aggregate_type, aggregate_id, payload, status, attempts, created_at, source_sequence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`, event.ID, event.Site, event.Type, event.Version, event.AggregateType, event.AggregateID, payload, StatusPending, createdAt, int64(41)); err != nil {
		t.Fatal("insert outbox event:", err)
	}
	backlog, err := (&Publisher{DB: database, Dialect: dialect}).Backlog(context.Background())
	if err != nil {
		t.Fatal("read MySQL outbox backlog metrics:", err)
	}
	if backlog.Pending != 1 || backlog.OldestUnpublishedAge < time.Minute {
		t.Fatalf("MySQL outbox backlog metrics = %+v, want one pending event older than a minute", backlog)
	}
	dispatcher := NewDispatcher(database, dialect)
	dispatcher.FailureThreshold = 1
	var calls int
	if err := dispatcher.RegisterTransactional("local-test", func(ctx context.Context, tx *sql.Tx, got contract.EventEnvelope) error {
		calls++
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_effect (event_id) VALUES (?)`, got.ID); err != nil {
			return err
		}
		if calls == 1 {
			return fmt.Errorf("simulate failure before commit")
		}
		return nil
	}); err != nil {
		t.Fatal("register transactional consumer:", err)
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("failed transaction Run = (%d, %v)", processed, err)
	}
	var receipts, effects int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox_receipt`).Scan(&receipts); err != nil {
		t.Fatal("count receipts after rollback:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM local_effect`).Scan(&effects); err != nil {
		t.Fatal("count effects after rollback:", err)
	}
	if receipts != 0 || effects != 0 {
		t.Fatalf("failed handler left receipt/effect rows = (%d, %d), want (0, 0)", receipts, effects)
	}
	failed, err := dispatcher.InspectQuarantined(context.Background(), event.Site, 10)
	if err != nil || len(failed) != 1 || failed[0].EventID != event.ID {
		t.Fatalf("quarantined delivery inspection = (%+v, %v)", failed, err)
	}
	if err := dispatcher.ReplayQuarantined(context.Background(), event.Site, "local-test", event.ID, func(context.Context, string, string, string) error { return nil }); err != nil {
		t.Fatal("authorized replay failed:", err)
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("retry transaction Run = (%d, %v)", processed, err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM _kora_outbox WHERE id = ?`, event.ID).Scan(&status); err != nil {
		t.Fatal("read publication status after local effect:", err)
	}
	if status != string(StatusPending) {
		t.Fatalf("local consumer changed publication status to %q; want pending until publisher ack", status)
	}
	publishedByProvider := false
	publisher := NewPublisher(database, eventPublisherFunc(func(_ context.Context, got contract.EventEnvelope) error {
		publishedByProvider = got.ID == event.ID
		return nil
	}), dialect)
	if count, err := publisher.PublishDue(context.Background(), 1); err != nil || count != 1 || !publishedByProvider {
		t.Fatalf("external publish after local effect = (%d, %v), provider saw event=%v", count, err, publishedByProvider)
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("duplicate transaction Run = (%d, %v)", processed, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox_receipt`).Scan(&receipts); err != nil {
		t.Fatal("count final receipts:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM local_effect`).Scan(&effects); err != nil {
		t.Fatal("count final effects:", err)
	}
	if receipts != 1 || effects != 1 || calls != 2 {
		t.Fatalf("receipt/effect/handler counts = (%d, %d, %d), want (1, 1, 2)", receipts, effects, calls)
	}
	var receiptSequence sql.NullInt64
	if err := database.QueryRow(`SELECT source_sequence FROM _kora_outbox_receipt WHERE consumer_name='local-test' AND event_id=?`, event.ID).Scan(&receiptSequence); err != nil {
		t.Fatal("read committed MySQL receipt source sequence:", err)
	}
	if !receiptSequence.Valid || receiptSequence.Int64 != 41 {
		t.Fatalf("committed MySQL receipt sequence = %+v, want 41", receiptSequence)
	}
	if err := database.QueryRow(`SELECT status FROM _kora_outbox WHERE id=?`, event.ID).Scan(&status); err != nil {
		t.Fatal("read final publication status:", err)
	}
	if status != string(StatusPublished) {
		t.Fatalf("publisher status=%q, want %q", status, StatusPublished)
	}

	concurrentEvent := contract.EventEnvelope{ID: contract.NewEventID(), Type: "request.created", Version: 1, Source: "engine", Site: "tenant-a",
		AggregateType: "request", AggregateID: contract.NewID(), OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{}`)}
	concurrentPayload, err := json.Marshal(concurrentEvent)
	if err != nil {
		t.Fatal("marshal concurrent event:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_outbox (id, site, event_type, event_version, aggregate_type, aggregate_id, payload, status, attempts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`, concurrentEvent.ID, concurrentEvent.Site, concurrentEvent.Type, concurrentEvent.Version, concurrentEvent.AggregateType, concurrentEvent.AggregateID, concurrentPayload, StatusPending, concurrentEvent.OccurredAt); err != nil {
		t.Fatal("insert concurrent outbox event:", err)
	}
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	var releaseProvider sync.Once
	release := func() { releaseProvider.Do(func() { close(providerRelease) }) }
	defer release()
	type publishResult struct {
		count int
		err   error
	}
	concurrentPublisher := NewPublisher(database, eventPublisherFunc(func(_ context.Context, got contract.EventEnvelope) error {
		close(providerEntered)
		<-providerRelease
		return nil
	}), dialect)
	publishDone := make(chan publishResult, 1)
	go func() {
		count, err := concurrentPublisher.PublishDue(context.Background(), 1)
		publishDone <- publishResult{count: count, err: err}
	}()
	select {
	case <-providerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("publisher did not reach the provider while holding its publication claim")
	}
	if err := database.QueryRow(`SELECT status FROM _kora_outbox WHERE id=?`, concurrentEvent.ID).Scan(&status); err != nil {
		t.Fatal("read in-flight publication status:", err)
	}
	if status != string(StatusPublishing) {
		t.Fatalf("in-flight publication status=%q, want %q", status, StatusPublishing)
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("local dispatch during provider publish = (%d, %v)", processed, err)
	}
	release()
	select {
	case result := <-publishDone:
		if result.err != nil || result.count != 1 {
			t.Fatalf("provider publish during local dispatch = (%d, %v)", result.count, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("publisher did not finish after provider acknowledgment")
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("local replay after concurrent completion = (%d, %v)", processed, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_outbox_receipt`).Scan(&receipts); err != nil {
		t.Fatal("count receipts after concurrent completion:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM local_effect`).Scan(&effects); err != nil {
		t.Fatal("count effects after concurrent completion:", err)
	}
	if receipts != 2 || effects != 2 || calls != 3 {
		t.Fatalf("concurrent receipt/effect/handler counts = (%d, %d, %d), want (2, 2, 3)", receipts, effects, calls)
	}
	if err := database.QueryRow(`SELECT status FROM _kora_outbox WHERE id=?`, concurrentEvent.ID).Scan(&status); err != nil {
		t.Fatal("read concurrent publication status:", err)
	}
	if status != string(StatusPublished) {
		t.Fatalf("concurrent final publication status=%q, want %q", status, StatusPublished)
	}
}
