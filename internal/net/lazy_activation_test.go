package net

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// A lazy activation keeps running when the request that started it is cancelled: the first activation after an
// upgrade bootstraps system tables, which can take longer than a client waits.
func TestLazyActivationOutlivesTheRequest(t *testing.T) {
	request, cancel := context.WithCancel(context.Background())
	cancel()
	var activationErr error
	var deadline time.Time
	err := (&SiteRouter{}).activateLazyDescriptor(request, contract.SiteDescriptor{SiteID: "site-1", Status: "active"}, func(ctx context.Context, _ string) error {
		activationErr = ctx.Err()
		deadline, _ = ctx.Deadline()
		return nil
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if activationErr != nil {
		t.Fatalf("activation context was cancelled with the request: %v", activationErr)
	}
	if until := time.Until(deadline); until <= 0 || until > lazyActivationTimeout {
		t.Fatalf("activation deadline in %v, want within %v", until, lazyActivationTimeout)
	}
}
