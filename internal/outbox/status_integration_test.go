//go:build integration

package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// TestOutboxStatusTransitionsOnRealDatabase drives one row through blocked,
// promotion, publication and operator replay on a real MySQL or PostgreSQL
// database. It checks the stored status and attempt count after every step.
func TestOutboxStatusTransitionsOnRealDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, env, driver string
		dialect           db.QueryDialect
	}{
		{"mysql", "KORA_OUTBOX_DISPOSABLE_MYSQL_DSN", "mysql", db.Resolve("mysql")},
		{"postgres", "KORA_OUTBOX_DISPOSABLE_POSTGRES_DSN", "postgres", db.Resolve("postgres")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s service", tc.env, tc.name)
			}
			database := openIsolatedDatabase(t, tc.driver, dsn)
			statements := db.OutboxTablesMySQL()
			if tc.driver == "postgres" {
				statements = db.OutboxTablesPostgres()
			}
			for _, statement := range statements {
				if _, err := database.Exec(statement); err != nil {
					t.Fatal("create outbox tables:", err)
				}
			}
			exercisePublisherStatusTransitions(t, database, tc.dialect)
		})
	}
}

// openIsolatedDatabase opens a fresh MySQL database or PostgreSQL schema on the
// disposable service and drops it when the test ends.
func openIsolatedDatabase(t *testing.T, driver, dsn string) *sql.DB {
	t.Helper()
	root, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable service:", err)
	}
	if err = root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("disposable %s unavailable: %v", driver, err)
	}
	name := fmt.Sprintf("kora_outbox_status_%d", time.Now().UnixNano())
	var isolated string
	if driver == "mysql" {
		if _, err = root.Exec("CREATE DATABASE `" + name + "`"); err != nil {
			_ = root.Close()
			t.Fatal("create isolated database:", err)
		}
		t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
		cfg, parseErr := mysql.ParseDSN(dsn)
		if parseErr != nil {
			t.Fatal("parse MySQL DSN:", parseErr)
		}
		cfg.DBName = name
		cfg.ParseTime = true
		isolated = cfg.FormatDSN()
	} else {
		if _, err = root.Exec(`CREATE SCHEMA "` + name + `"`); err != nil {
			_ = root.Close()
			t.Fatal("create isolated schema:", err)
		}
		t.Cleanup(func() { _, _ = root.Exec(`DROP SCHEMA "` + name + `" CASCADE`); _ = root.Close() })
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal("parse PostgreSQL DSN:", parseErr)
		}
		query := parsed.Query()
		query.Set("search_path", name)
		parsed.RawQuery = query.Encode()
		isolated = parsed.String()
	}
	database, err := sql.Open(driver, isolated)
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func exercisePublisherStatusTransitions(t *testing.T, database *sql.DB, dialect db.QueryDialect) {
	ctx := context.Background()
	site := fmt.Sprintf("status-it-%d", time.Now().UnixNano())
	event := contract.EventEnvelope{
		ID: contract.NewEventID(), Type: "kora.invoice.after_insert", Version: 1, Source: "kora.kernel",
		Site: site, AggregateType: "Invoice", AggregateID: "INV-1", OccurredAt: time.Now().UTC(),
		Data: contract.MustEncodeData(map[string]any{"doctype": "Invoice"}),
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin append:", err)
	}
	if err := NewSQLWriter(dialect).Append(ctx, tx, event); err != nil {
		_ = tx.Rollback()
		t.Fatal("append event:", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit append:", err)
	}
	stored := func() (Status, int) {
		var status string
		var attempts int
		if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT status, attempts FROM _kora_outbox WHERE id = ?`), event.ID).Scan(&status, &attempts); err != nil {
			t.Fatal("read row:", err)
		}
		return Status(status), attempts
	}
	want := func(step string, status Status, attempts int) {
		t.Helper()
		if got, gotAttempts := stored(); got != status || gotAttempts != attempts {
			t.Fatalf("%s: status/attempts = %s/%d, want %s/%d", step, got, gotAttempts, status, attempts)
		}
	}
	// Destination refusal for capacity parks the row without consuming the attempt.
	full := eventPublisherFunc(func(context.Context, contract.EventEnvelope) error {
		return fmt.Errorf("stream at limit: %w", contract.ErrDestinationFull)
	})
	publisher := NewPublisher(database, full, dialect)
	if _, err := publisher.PublishDue(ctx, 10); err != nil {
		t.Fatal("publish against full destination:", err)
	}
	want("after capacity refusal", StatusBlocked, 0)
	var retryAt db.Timestamp
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT next_attempt_at FROM _kora_outbox WHERE id = ?`), event.ID).Scan(&retryAt); err != nil {
		t.Fatal("read blocked retry time:", err)
	}
	if delay := time.Until(retryAt.Time); delay < blockedRetryDelay-10*time.Second || delay > blockedRetryDelay {
		t.Fatalf("blocked retry delay = %s, want about %s", delay, blockedRetryDelay)
	}

	// A blocked row is not due until its retry time; the publisher leaves it alone.
	if n, err := publisher.PublishDue(ctx, 10); err != nil || n != 0 {
		t.Fatalf("publish before retry time = (%d, %v), want (0, nil)", n, err)
	}
	want("before retry time", StatusBlocked, 0)

	// Once due, the blocked row is promoted and published on the normal path.
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_outbox SET next_attempt_at = ? WHERE id = ?`), time.Now().UTC().Add(-time.Minute), event.ID); err != nil {
		t.Fatal("make blocked row due:", err)
	}
	accepted := eventPublisherFunc(func(context.Context, contract.EventEnvelope) error { return nil })
	if n, err := NewPublisher(database, accepted, dialect).PublishDue(ctx, 10); err != nil || n != 1 {
		t.Fatalf("publish after capacity returns = (%d, %v), want (1, nil)", n, err)
	}
	want("after publication", StatusPublished, 1)

	// Operator replay requeues a row that exhausted its attempts and resets the count.
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_outbox SET status = ?, attempts = ? WHERE id = ?`), string(StatusFailed), 8, event.ID); err != nil {
		t.Fatal("mark row failed:", err)
	}
	if n, err := publisher.ReplayFailed(ctx); err != nil || n != 1 {
		t.Fatalf("ReplayFailed = (%d, %v), want (1, nil)", n, err)
	}
	want("after replay", StatusPending, 0)
}
