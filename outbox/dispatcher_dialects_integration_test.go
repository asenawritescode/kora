//go:build integration

package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestPostgresTransactionalConsumerReceiptAndPublication(t *testing.T) {
	dsn := os.Getenv("KORA_OUTBOX_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_OUTBOX_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL service")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL service:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable PostgreSQL service:", err)
	}
	schemaName := fmt.Sprintf("kora_outbox_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated outbox schema:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`)
		_ = root.Close()
	})
	parsedDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse disposable PostgreSQL DSN:", err)
	}
	query := parsedDSN.Query()
	query.Set("search_path", schemaName)
	parsedDSN.RawQuery = query.Encode()
	database, err := sql.Open("postgres", parsedDSN.String())
	if err != nil {
		t.Fatal("open isolated outbox schema:", err)
	}
	database.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping isolated outbox schema:", err)
	}
	for _, statement := range db.OutboxTablesPostgres() {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create PostgreSQL outbox schema: %v\n%s", err, statement)
		}
	}
	assertTenantSequenceMigration(t, database, db.Resolve("postgres"), "postgres")
	if _, err := database.Exec(`CREATE TABLE local_effect (event_id VARCHAR(64) PRIMARY KEY)`); err != nil {
		t.Fatal("create local effect table:", err)
	}
	runTransactionalReceiptScenario(t, database, db.Resolve("postgres"), "postgres")
}

func TestLibSQLTransactionalConsumerReceiptAndPublication(t *testing.T) {
	dsn := os.Getenv("KORA_OUTBOX_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_OUTBOX_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL endpoint")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL endpoint:", err)
	}
	database.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL endpoint:", err)
	}
	for _, statement := range db.OutboxTablesLibSQL() {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create LibSQL outbox schema: %v\n%s", err, statement)
		}
	}
	assertTenantSequenceMigration(t, database, db.Resolve("libsql"), "libsql")
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS local_effect (event_id VARCHAR(64) PRIMARY KEY)`); err != nil {
		t.Fatal("create local effect table:", err)
	}
	runTransactionalReceiptScenario(t, database, db.Resolve("libsql"), "libsql")
}

func assertTenantSequenceMigration(t *testing.T, database *sql.DB, dialect db.QueryDialect, label string) {
	t.Helper()
	if _, err := database.Exec(db.Rebind(dialect,
		`INSERT INTO _kora_event_sequence (site, last_sequence) VALUES (?, ?)`), "legacy-route", 8); err != nil {
		t.Fatalf("%s seed legacy route sequence: %v", label, err)
	}
	writer := NewSQLWriter(dialect)
	ids := make([]string, 0, 2)
	for index, site := range []string{"renamed-route", "second-alias"} {
		tx, err := database.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s begin tenant sequence transaction: %v", label, err)
		}
		event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "record.updated", Version: contract.CurrentVersion,
			Source: "test", Site: site, OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{}`)}
		if err := writer.Append(context.Background(), tx, event); err != nil {
			_ = tx.Rollback()
			t.Fatalf("%s append tenant sequence event: %v", label, err)
		}
		ids = append(ids, event.ID)
		if err := tx.Commit(); err != nil {
			t.Fatalf("%s commit tenant sequence event: %v", label, err)
		}
		var payload string
		if err := database.QueryRow(db.Rebind(dialect, `SELECT payload FROM _kora_outbox WHERE id = ?`), event.ID).Scan(&payload); err != nil {
			t.Fatalf("%s read tenant sequence payload: %v", label, err)
		}
		var stored contract.EventEnvelope
		if err := json.Unmarshal([]byte(payload), &stored); err != nil {
			t.Fatalf("%s decode tenant sequence payload: %v", label, err)
		}
		want := fmt.Sprint(9 + index)
		if string(stored.Sequence) != want || stored.Site != site {
			t.Fatalf("%s event sequence/site = %q/%q, want %q/%q", label, stored.Sequence, stored.Site, want, site)
		}
	}
	for _, id := range ids {
		if _, err := database.Exec(db.Rebind(dialect, `DELETE FROM _kora_outbox WHERE id = ?`), id); err != nil {
			t.Fatalf("%s remove sequence migration fixture event: %v", label, err)
		}
	}
}

func runTransactionalReceiptScenario(t *testing.T, database *sql.DB, dialect db.QueryDialect, label string) {
	t.Helper()
	site := label + "-" + contract.NewID()
	createdAt := time.Now().UTC().Add(-2 * time.Minute)
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "request.created", Version: 1, Source: "engine", Site: site,
		Sequence: "37", AggregateType: "request", AggregateID: contract.NewID(), OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{}`)}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal("marshal event:", err)
	}
	if _, err := database.Exec(db.Rebind(dialect, `INSERT INTO _kora_outbox
		(id, site, event_type, event_version, aggregate_type, aggregate_id, payload, status, attempts, created_at, source_sequence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`), event.ID, event.Site, event.Type, event.Version, event.AggregateType, event.AggregateID, payload, StatusPending, createdAt, int64(37)); err != nil {
		t.Fatal("insert durable event:", err)
	}
	backlog, err := (&Publisher{DB: database, Dialect: dialect}).Backlog(context.Background())
	if err != nil {
		t.Fatalf("read %s outbox backlog metrics: %v", label, err)
	}
	if backlog.Pending != 1 || backlog.OldestUnpublishedAge < time.Minute {
		t.Fatalf("%s outbox backlog metrics = %+v, want one pending event older than a minute", label, backlog)
	}
	dispatcher := NewDispatcher(database, dialect)
	dispatcher.FailureThreshold = 1
	var calls int
	consumerName := label + "-consumer"
	if err := dispatcher.RegisterTransactional(consumerName, func(ctx context.Context, tx *sql.Tx, got contract.EventEnvelope) error {
		calls++
		if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO local_effect (event_id) VALUES (?)`), got.ID); err != nil {
			return err
		}
		if calls == 1 {
			return fmt.Errorf("injected handler failure")
		}
		return nil
	}); err != nil {
		t.Fatal("register consumer:", err)
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("failed local transaction = (%d, %v), want (0, nil)", processed, err)
	}
	var receipts, effects int
	if err := database.QueryRow(db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_outbox_receipt WHERE consumer_name = ? AND event_id = ?`), consumerName, event.ID).Scan(&receipts); err != nil {
		t.Fatal("count receipts after rollback:", err)
	}
	if err := database.QueryRow(db.Rebind(dialect, `SELECT COUNT(*) FROM local_effect WHERE event_id = ?`), event.ID).Scan(&effects); err != nil {
		t.Fatal("count effects after rollback:", err)
	}
	if receipts != 0 || effects != 0 {
		t.Fatalf("failed handler left receipt/effect rows (%d, %d), want (0, 0)", receipts, effects)
	}
	quarantined, err := dispatcher.InspectQuarantined(context.Background(), site, 10)
	if err != nil || len(quarantined) != 1 || quarantined[0].EventID != event.ID || quarantined[0].Attempts != 1 {
		t.Fatalf("quarantined delivery inspection = (%+v, %v)", quarantined, err)
	}
	if err := dispatcher.ReplayQuarantined(context.Background(), site, consumerName, event.ID, func(context.Context, string, string, string) error { return nil }); err != nil {
		t.Fatal("authorized replay reset failed delivery:", err)
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("retry local transaction = (%d, %v), want (1, nil)", processed, err)
	}
	var status string
	if err := database.QueryRow(db.Rebind(dialect, `SELECT status FROM _kora_outbox WHERE id = ?`), event.ID).Scan(&status); err != nil {
		t.Fatal("read publication status after local commit:", err)
	}
	if status != string(StatusPending) {
		t.Fatalf("local receipt changed provider publication state to %q, want pending", status)
	}
	providerCalls := 0
	publisher := NewPublisher(database, eventPublisherFunc(func(_ context.Context, got contract.EventEnvelope) error {
		providerCalls++
		if got.ID != event.ID {
			t.Errorf("provider got event ID %q, want %q", got.ID, event.ID)
		}
		return nil
	}), dialect)
	if count, err := publisher.PublishDue(context.Background(), 1); err != nil || count != 1 || providerCalls != 1 {
		t.Fatalf("provider acknowledgment = (%d, %v), calls=%d; want (1, nil), 1", count, err, providerCalls)
	}
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("duplicate local delivery = (%d, %v), want (0, nil)", processed, err)
	}
	if err := database.QueryRow(db.Rebind(dialect, `SELECT COUNT(*) FROM _kora_outbox_receipt WHERE consumer_name = ? AND event_id = ?`), consumerName, event.ID).Scan(&receipts); err != nil {
		t.Fatal("count committed receipt:", err)
	}
	if err := database.QueryRow(db.Rebind(dialect, `SELECT COUNT(*) FROM local_effect WHERE event_id = ?`), event.ID).Scan(&effects); err != nil {
		t.Fatal("count committed effect:", err)
	}
	if err := database.QueryRow(db.Rebind(dialect, `SELECT status FROM _kora_outbox WHERE id = ?`), event.ID).Scan(&status); err != nil {
		t.Fatal("read final provider status:", err)
	}
	if receipts != 1 || effects != 1 || calls != 2 || status != string(StatusPublished) {
		t.Fatalf("final state receipts=%d effects=%d calls=%d status=%q; want 1/1/2/published", receipts, effects, calls, status)
	}
	var receiptSequence sql.NullInt64
	if err := database.QueryRow(db.Rebind(dialect, `SELECT source_sequence FROM _kora_outbox_receipt WHERE consumer_name = ? AND event_id = ?`), consumerName, event.ID).Scan(&receiptSequence); err != nil {
		t.Fatalf("read committed %s receipt sequence: %v", label, err)
	}
	if !receiptSequence.Valid || receiptSequence.Int64 != 37 {
		t.Fatalf("committed %s receipt sequence = %+v, want 37", label, receiptSequence)
	}
}
