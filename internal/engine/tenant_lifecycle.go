package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/internal/tenantruntime"
	knet "github.com/asenawritescode/kora/net"
)

// DrainRetiredTenantRuntime retries a timed drain without closing resources
// beneath an admitted request. The caller must retain and join its goroutine.
func DrainRetiredTenantRuntime(runtime *knet.LoadedSite, timeout time.Duration) error {
	if runtime == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := runtime.Close(ctx); err != nil {
		slog.Warn("retired tenant runtime did not drain before shutdown deadline", "tenant", runtime.Name, "error", err)
		if err := runtime.Close(context.Background()); err != nil {
			slog.Error("retired tenant runtime close failed", "tenant", runtime.Name, "error", err)
			return err
		}
	}
	return nil
}

// TransferStableTenantResources gives the newly published generation ownership
// of the database and session manager shared with its retired predecessor.
func TransferStableTenantResources(retired, active *knet.LoadedSite) bool {
	if retired == nil || active == nil || retired.DB == nil || retired.DB != active.DB {
		return false
	}
	retired.DB = nil
	retired.SessionManager = nil
	return true
}

// StopTenantServiceBundle stops request-visible sidecars before closing their
// event bus. The runtime generation remains responsible for joining its own
// registered worker loops.
func StopTenantServiceBundle(services tenantruntime.Services) {
	if services.WebhookWorker != nil {
		services.WebhookWorker.Stop()
	}
	if services.AnalyticsRelay != nil {
		services.AnalyticsRelay.Stop()
	}
	if bus, ok := services.EventBus.(interface{ Close() error }); ok {
		_ = bus.Close()
	}
}
