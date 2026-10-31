//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/scheduler"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestScheduledOccurrenceLeaseAndUncertainOutcomeAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		driver, env string
		dialect     db.Dialect
	}{
		{"mysql", "KORA_SCHEDULER_DISPOSABLE_MYSQL_DSN", &db.MySQLDialect{}},
		{"postgres", "KORA_SCHEDULER_DISPOSABLE_POSTGRES_DSN", &db.PostgresDialect{}},
		{"libsql", "KORA_SCHEDULER_DISPOSABLE_LIBSQL_DSN", &db.LibSQLDialect{}},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip("set " + tc.env + " to a fresh disposable database")
			}
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("open disposable database:", err)
			}
			defer database.Close()
			database.SetMaxOpenConns(4)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := database.PingContext(ctx); err != nil {
				t.Fatal("ping disposable database:", err)
			}
			store, err := New(database, tc.dialect)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Ensure(ctx); err != nil {
				t.Fatal("ensure scheduler schema:", err)
			}
			tenant := "scheduler-test-" + time.Now().UTC().Format("20060102150405.000000000")
			defer func() {
				_, _ = database.ExecContext(context.Background(), db.Rebind(tc.dialect, `DELETE FROM _kora_schedule_occurrence WHERE tenant_id=?`), tenant)
				_, _ = database.ExecContext(context.Background(), db.Rebind(tc.dialect, `DELETE FROM _kora_schedule_definition WHERE tenant_id=?`), tenant)
			}()
			now := time.Now().UTC().Truncate(time.Microsecond)
			job := &scheduler.JobConfig{Name: "hourly", Type: scheduler.JobWebhook, Schedule: "0 * * * *", Config: map[string]any{"recipient": "test"}}
			first := now.Add(-time.Minute)
			if err := store.RegisterDefinition(ctx, tenant, job, scheduler.JobDefinitionDigest(job), first, now); err != nil {
				t.Fatal("register schedule:", err)
			}
			if got, err := store.NextRunAt(ctx, tenant, job.Name); err != nil || !got.Equal(first) {
				t.Fatalf("next run after registration = %s, %v; want %s", got, err, first)
			}
			claimed, err := store.ClaimDue(ctx, tenant, job.Name, "worker-a", now, time.Minute)
			if err != nil {
				t.Fatal("claim first occurrence:", err)
			}
			if claimed == nil || claimed.Owner != "worker-a" || claimed.Fence != 1 || claimed.Attempts != 1 || claimed.ID == "" {
				t.Fatalf("unexpected initial claim: %+v", claimed)
			}
			if second, err := store.ClaimDue(ctx, tenant, job.Name, "worker-b", now, time.Minute); err != nil || second != nil {
				t.Fatalf("second worker claim = %+v, %v; want no concurrent claim", second, err)
			}
			if err := store.Renew(ctx, *claimed, now.Add(20*time.Second), time.Minute); err != nil {
				t.Fatal("renew claim:", err)
			}
			if err := store.Complete(ctx, *claimed, now.Add(time.Hour), now.Add(30*time.Second)); err != nil {
				t.Fatal("complete occurrence:", err)
			}
			if got, err := store.NextRunAt(ctx, tenant, job.Name); err != nil || !got.Equal(now.Add(time.Hour)) {
				t.Fatalf("next run after completion = %s, %v; want %s", got, err, now.Add(time.Hour))
			}
			if next, err := store.ClaimDue(ctx, tenant, job.Name, "worker-a", now.Add(31*time.Second), time.Minute); err != nil || next != nil {
				t.Fatalf("unexpected duplicate occurrence after completion: %+v, %v", next, err)
			}

			pendingJob := &scheduler.JobConfig{Name: "pending-metrics", Type: scheduler.JobWebhook, Schedule: "0 * * * *"}
			if err := store.RegisterDefinition(ctx, tenant, pendingJob, scheduler.JobDefinitionDigest(pendingJob), first, now); err != nil {
				t.Fatal("register pending metrics schedule:", err)
			}
			pending, err := store.ClaimDue(ctx, tenant, pendingJob.Name, "worker-metrics", now, time.Minute)
			if err != nil || pending == nil {
				t.Fatalf("materialize pending metrics occurrence: %+v, %v", pending, err)
			}
			if _, err := database.ExecContext(ctx, db.Rebind(tc.dialect, `UPDATE _kora_schedule_occurrence SET state='pending',owner='',lease_until=NULL WHERE tenant_id=? AND job_name=? AND scheduled_at=?`), tenant, pendingJob.Name, encodeTime(tc.driver, pending.ScheduledAt)); err != nil {
				t.Fatal("restore occurrence to pending for metrics probe:", err)
			}
			pendingCount, oldestPending, err := store.BacklogMetrics(ctx, tenant)
			if err != nil {
				t.Fatal("read durable scheduler backlog metrics:", err)
			}
			if pendingCount != 1 || !oldestPending.Equal(pending.ScheduledAt.UTC()) {
				t.Fatalf("durable scheduler backlog = (%d, %s), want (1, %s)", pendingCount, oldestPending, pending.ScheduledAt.UTC())
			}

			uncertainJob := &scheduler.JobConfig{Name: "uncertain", Type: scheduler.JobWebhook, Schedule: "0 * * * *"}
			if err := store.RegisterDefinition(ctx, tenant, uncertainJob, scheduler.JobDefinitionDigest(uncertainJob), first, now); err != nil {
				t.Fatal("register uncertain schedule:", err)
			}
			uncertain, err := store.ClaimDue(ctx, tenant, uncertainJob.Name, "worker-a", now, time.Minute)
			if err != nil || uncertain == nil {
				t.Fatalf("claim uncertain test occurrence: %+v, %v", uncertain, err)
			}
			_, err = database.ExecContext(ctx, db.Rebind(tc.dialect, `UPDATE _kora_schedule_occurrence SET lease_until=? WHERE tenant_id=? AND job_name=? AND scheduled_at=?`), encodeTime(tc.driver, now.Add(-time.Second)), tenant, uncertainJob.Name, encodeTime(tc.driver, uncertain.ScheduledAt))
			if err != nil {
				t.Fatal("expire test claim:", err)
			}
			if err := store.Complete(ctx, *uncertain, now.Add(time.Hour), now); !errors.Is(err, scheduler.ErrScheduleLeaseLost) {
				t.Fatalf("expired worker completion error = %v, want ErrScheduleLeaseLost", err)
			}
			if _, err := store.ClaimDue(ctx, tenant, uncertainJob.Name, "worker-b", now, time.Minute); !errors.Is(err, scheduler.ErrScheduleOutcomeUnknown) {
				t.Fatalf("expired outcome error = %v, want ErrScheduleOutcomeUnknown", err)
			}
			var state string
			if err := database.QueryRowContext(ctx, db.Rebind(tc.dialect, `SELECT state FROM _kora_schedule_occurrence WHERE tenant_id=? AND job_name=? AND scheduled_at=?`), tenant, uncertainJob.Name, encodeTime(tc.driver, uncertain.ScheduledAt)).Scan(&state); err != nil {
				t.Fatal("read uncertain state:", err)
			}
			if state != "outcome_unknown" {
				t.Fatalf("expired occurrence state = %q, want outcome_unknown", state)
			}

			parallelJob := &scheduler.JobConfig{Name: "parallel", Type: scheduler.JobWebhook, Schedule: "0 * * * *"}
			if err := store.RegisterDefinition(ctx, tenant, parallelJob, scheduler.JobDefinitionDigest(parallelJob), first, now); err != nil {
				t.Fatal("register parallel schedule:", err)
			}
			start := make(chan struct{})
			type claimResult struct {
				occurrence *scheduler.ScheduleOccurrence
				err        error
			}
			results := make(chan claimResult, 2)
			var wg sync.WaitGroup
			for _, owner := range []string{"parallel-a", "parallel-b"} {
				wg.Add(1)
				go func(owner string) {
					defer wg.Done()
					<-start
					occurrence, err := store.ClaimDue(ctx, tenant, parallelJob.Name, owner, now, time.Minute)
					results <- claimResult{occurrence: occurrence, err: err}
				}(owner)
			}
			close(start)
			wg.Wait()
			close(results)
			claimedCount := 0
			for result := range results {
				if result.err != nil {
					t.Errorf("parallel claim failed: %v", result.err)
				}
				if result.occurrence != nil {
					claimedCount++
				}
			}
			if claimedCount != 1 {
				t.Fatalf("concurrent workers claimed %d occurrences, want exactly one", claimedCount)
			}
		})
	}
}
