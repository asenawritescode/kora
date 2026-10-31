package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/site"
)

func TestCloseOnboardingJobsWaitsAndRejectsNewJobs(t *testing.T) {
	handler := &ConsoleHandler{}
	handler.jobLifecycleMu.Lock()
	handler.jobCtx, handler.jobCancel = context.WithCancel(context.Background())
	handler.jobWorkers.Add(1)
	handler.jobLifecycleMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := handler.CloseOnboardingJobs(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseOnboardingJobs error = %v, want deadline", err)
	}

	handler.jobWorkers.Done()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.CloseOnboardingJobs(ctx); err != nil {
		t.Fatalf("retry CloseOnboardingJobs: %v", err)
	}
	if handler.scheduleOnboard(site.OnboardingJob{ID: "after-close"}, onboardRequest{}) {
		t.Fatal("onboarding job was admitted after close began")
	}
}
