package engine

import (
	"context"
	"testing"

	"github.com/asenawritescode/kora/scheduler"
)

func TestStartSchedulerRequiresDependenciesWhenJobsConfigured(t *testing.T) {
	worker, err := StartScheduler(context.Background(), SchedulerOptions{Jobs: []*scheduler.JobConfig{{Name: "example", Type: "custom", Schedule: "0 0 * * *"}}})
	if err == nil || worker != nil {
		t.Fatalf("StartScheduler = (%v, %v), want dependency error and no worker", worker, err)
	}
}

func TestStartSchedulerTreatsEmptyDefinitionsAsDisabled(t *testing.T) {
	worker, err := StartScheduler(context.Background(), SchedulerOptions{})
	if err != nil || worker != nil {
		t.Fatalf("StartScheduler = (%v, %v), want disabled scheduler", worker, err)
	}
}
