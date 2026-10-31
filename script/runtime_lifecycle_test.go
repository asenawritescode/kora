package script

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunnerBoundsWaitersAndCloseJoinsExecutions(t *testing.T) {
	runner := NewEmbeddedRunner(EmbeddedConfig{PoolSize: 1, Timeout: time.Minute})
	results := make(chan error, 2)
	run := func() {
		_, err := runner.Execute(context.Background(), ExecuteRequest{Script: "for (;;) {}"})
		results <- err
	}
	go run()
	time.Sleep(30 * time.Millisecond)
	go run() // at most one active and one waiting call are admitted
	time.Sleep(10 * time.Millisecond)
	if _, err := runner.Execute(context.Background(), ExecuteRequest{Script: "1 + 1"}); err == nil || !strings.Contains(err.Error(), "at capacity") {
		t.Fatalf("third concurrent call error = %v, want bounded-capacity error", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- runner.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel and join active/waiting executions")
	}
	for range 2 {
		select {
		case <-results:
		case <-time.After(2 * time.Second):
			t.Fatal("script execution did not return after runner shutdown")
		}
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
}
