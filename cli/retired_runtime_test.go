package cli

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/engine"
	knet "github.com/asenawritescode/kora/net"
)

func TestRetiredRuntimeKeepsDatabaseUntilRequestsDrain(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	runtime := &knet.LoadedSite{Name: "draining-tenant", DB: database}
	handle, ok := runtime.AcquireRuntime()
	if !ok {
		t.Fatal("could not acquire runtime handle")
	}
	runtime.RetireRuntime()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = engine.DrainRetiredTenantRuntime(runtime, 10*time.Millisecond)
	}()
	select {
	case <-done:
		t.Fatal("retirement closed while request handle remained active")
	case <-time.After(50 * time.Millisecond):
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("database closed before request release: %v", err)
	}
	handle.Release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retirement did not finish after request release")
	}
	if err := database.Ping(); err == nil {
		t.Fatal("retired database remained open after drain")
	}
}
