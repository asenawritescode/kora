//go:build integration

package facts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
)

// TestJobStoreLifecycle drives rebuild jobs through claim, lease expiry and takeover, fenced
// writes, failure and cancel against a real database on each dialect.
func TestJobStoreLifecycle(t *testing.T) {
	tests := []struct {
		driver, envVar string
	}{
		{"mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			ctx := context.Background()
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := database.DB.ExecContext(ctx, db.Rebind(dialect, query), args...); err != nil {
					t.Fatalf("exec %q: %v", query, err)
				}
			}
			exec("CREATE TABLE _kora_tenant_event_sequence (id INT PRIMARY KEY, last_sequence BIGINT NOT NULL DEFAULT 0)")
			exec("INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 41)")
			store := &JobStore{DB: database.DB, Dialect: dialect}

			// Enqueue captures the outbox sequence at the start.
			first, err := store.Enqueue(ctx, "Sale")
			if err != nil {
				t.Fatal(err)
			}
			if first.Status != JobQueued || first.StartSequence != 41 {
				t.Fatalf("enqueued = %+v, want queued at sequence 41", first)
			}

			// A fresh lease blocks a second claim; the first claim gets fencing token 1.
			a, err := store.Claim(ctx, "Sale", "worker-a")
			if err != nil || a == nil || a.token != 1 || a.Status != JobRunning {
				t.Fatalf("claim a = %+v, %v; want running with token 1", a, err)
			}
			other, err := store.Claim(ctx, "Sale", "worker-b")
			if err != nil || other != nil {
				t.Fatalf("claim b while leased = %+v, %v; want nil", other, err)
			}
			if err = store.Renew(ctx, a); err != nil {
				t.Fatalf("renew a: %v", err)
			}

			// Simulated crash: the lease expires on the database clock and worker b takes over.
			exec(fmt.Sprintf("UPDATE %s SET lease_until = %s WHERE id = ?", dialect.QuoteIdent(jobTable), dialect.NowMinusSeconds()), 120, first.ID)
			b, err := store.Claim(ctx, "Sale", "worker-b")
			if err != nil || b == nil || b.token != 2 {
				t.Fatalf("takeover = %+v, %v; want token 2", b, err)
			}
			if err = store.Renew(ctx, a); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("renew after takeover = %v, want ErrLeaseLost", err)
			}
			if err = store.Finish(ctx, a, JobDone, ""); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("finish after takeover = %v, want ErrLeaseLost", err)
			}
			if err = store.Finish(ctx, b, JobQueued, ""); err == nil {
				t.Fatal("finish to queued was accepted")
			}
			if err = store.Finish(ctx, b, JobDone, ""); err != nil {
				t.Fatalf("finish b: %v", err)
			}
			assertJob(t, store, first.ID, JobDone, "")

			// Failure keeps its message; cancel moves a running job and fences its worker.
			failed, err := store.Enqueue(ctx, "Sale")
			if err != nil {
				t.Fatal(err)
			}
			c, err := store.Claim(ctx, "Sale", "worker-c")
			if err != nil || c == nil || c.ID != failed.ID {
				t.Fatalf("claim c = %+v, %v", c, err)
			}
			if err = store.Finish(ctx, c, JobFailed, "boom"); err != nil {
				t.Fatalf("finish failed: %v", err)
			}
			assertJob(t, store, failed.ID, JobFailed, "boom")

			cancelled, err := store.Enqueue(ctx, "Sale")
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Cancel(ctx, cancelled.ID); err == nil {
				t.Fatal("cancel of a queued job was accepted")
			}
			d, err := store.Claim(ctx, "Sale", "worker-d")
			if err != nil || d == nil {
				t.Fatalf("claim d = %+v, %v", d, err)
			}
			if err = store.Cancel(ctx, cancelled.ID); err != nil {
				t.Fatalf("cancel running: %v", err)
			}
			if err = store.Renew(ctx, d); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("renew after cancel = %v, want ErrLeaseLost", err)
			}
			assertJob(t, store, cancelled.ID, JobCancelled, "")
		})
	}
}

func assertJob(t *testing.T, store *JobStore, id string, want JobStatus, errText string) {
	t.Helper()
	job, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != want || job.Error != errText {
		t.Fatalf("job %s = %s %q, want %s %q", id, job.Status, job.Error, want, errText)
	}
}
