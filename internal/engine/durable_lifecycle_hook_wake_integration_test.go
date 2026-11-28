//go:build integration

package engine

import (
	"context"
	"database/sql"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/sweep"
)

// countingAdmission counts hook passes: every pass acquires work admission once.
type countingAdmission struct{ passes atomic.Int32 }

func (a *countingAdmission) Acquire(context.Context) (func(), error) {
	a.passes.Add(1)
	return func() {}, nil
}

// TestDurableLifecycleHookWorkerIdleRunsNoPassUntilWoken runs an idle hook
// worker with a one-hour sweep. A timer-driven worker would pass every second;
// the wake-driven worker passes once at start and once per wake.
func TestDurableLifecycleHookWorkerIdleRunsNoPassUntilWoken(t *testing.T) {
	cases := []struct{ driver, env string }{
		{"mysql", "KORA_ENGINE_HOOK_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_ENGINE_HOOK_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		dsn := os.Getenv(tc.env)
		if dsn == "" {
			t.Logf("skip %s: set %s to a fresh disposable database", tc.driver, tc.env)
			continue
		}
		t.Run(tc.driver, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("open disposable database:", err)
			}
			t.Cleanup(func() { _ = database.Close() })
			dialect := kdb.Resolve(tc.driver)
			for _, statement := range durableLifecycleFixtureDDL(tc.driver) {
				if _, err := database.ExecContext(ctx, statement); err != nil {
					t.Fatalf("create durable lifecycle fixture: %v\n%s", err, statement)
				}
			}
			store := &script.Store{DB: database, Dialect: dialect}
			if err := store.EnsureAsyncHookInvocationSchema(ctx); err != nil {
				t.Fatal("ensure lifecycle invocation table:", err)
			}
			tenant := contract.TenantID("hook-wake-" + contract.NewID())
			admission := &countingAdmission{}
			wake := sweep.NewWake()
			worker := LifecycleHookWorker{
				Runner: &integrationMutationLifecycleRunner{}, Store: store, DB: database,
				Registry: doctype.NewRegistry(), TenantID: string(tenant), Site: string(tenant), DBType: tc.driver,
				Admission: admission, Wake: wake, Sweep: time.Hour,
			}
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				RunDurableLifecycleHookWorker(runCtx, worker)
			}()
			time.Sleep(2500 * time.Millisecond)
			if got := admission.passes.Load(); got != 1 {
				t.Fatalf("idle worker passes after 2.5s = %d, want 1 (start only)", got)
			}
			wake.Signal()
			time.Sleep(1500 * time.Millisecond)
			if got := admission.passes.Load(); got != 2 {
				t.Fatalf("passes after one wake = %d, want 2", got)
			}
			stop()
			<-done
		})
	}
}
