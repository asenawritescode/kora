//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// TestLibSQLRequestSchema uses the real LibSQL driver and only runs when an
// operator explicitly supplies a disposable test database. It is additive and
// repeats the migration to prove idempotence; it never drops user data.
func TestLibSQLRequestSchema(t *testing.T) {
	dsn := os.Getenv("KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_REQUESTS_DISPOSABLE_LIBSQL_DSN to a disposable LibSQL database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable LibSQL database:", err)
	}
	statements, err := SchemaStatements(db.Resolve("libsql"))
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		for i, statement := range statements {
			if _, err := database.ExecContext(ctx, statement); err != nil {
				t.Fatalf("migration pass %d statement %d: %v", pass+1, i+1, err)
			}
		}
	}
	for _, table := range []string{"_kora_request", "_kora_request_assignment", "_kora_request_decision", "_kora_request_history", "_kora_request_read", "_kora_request_idempotency"} {
		var found string
		if err := database.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&found); err != nil {
			t.Fatalf("request table %s missing after migration: %v", table, err)
		}
	}
	store, err := New(database, db.Resolve("libsql"))
	if err != nil {
		t.Fatal("construct request store:", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := contract.RequestID("01K7LIBSQLREQUEST000000000000")
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_request
		(id, tenant_id, revision, kind, assignment_mode, status, requester_id, title, message, subject_digest, policy_id,
		 policy_revision, information_schema_id, information_schema_version, source_instance_id, source_step_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', 0, '', '', ?, ?)`,
		string(id), "tenant-libsql", 1, string(contract.RequestReview), string(contract.AssignmentAny), string(contract.RequestPending), "requester-libsql",
		"LibSQL request", "Read this request", "", now, now); err != nil {
		t.Fatal("insert LibSQL request fixture:", err)
	}
	loaded, err := store.Get(ctx, "tenant-libsql", id)
	if err != nil {
		t.Fatal("load request through LibSQL store:", err)
	}
	if loaded.ID != id || loaded.CreatedAt.IsZero() || !loaded.CreatedAt.Equal(now) {
		t.Fatalf("LibSQL request round trip = %#v", loaded)
	}
}
