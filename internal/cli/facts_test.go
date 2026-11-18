package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/facts"
)

// TestAdmitFactsMaintenance checks that a full maintenance lane fails at once without --wait, and that
// --wait retries until a slot frees or the context ends.
func TestAdmitFactsMaintenance(t *testing.T) {
	old := factsRetryInterval
	factsRetryInterval = time.Millisecond
	t.Cleanup(func() { factsRetryInterval = old })
	release := func() {}
	tests := []struct {
		name      string
		wait      bool
		exhausted int
		cancel    bool
		wantCalls int
		wantErr   error
	}{
		{name: "full lane fails without wait", exhausted: 5, wantCalls: 1, wantErr: dbaccess.ErrExhausted},
		{name: "wait retries until admitted", wait: true, exhausted: 2, wantCalls: 3},
		{name: "wait stops when the context ends", wait: true, exhausted: 1 << 30, cancel: true, wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			calls := 0
			got, err := admitFactsMaintenance(ctx, tc.wait, func(context.Context) (func(), error) {
				calls++
				if calls <= tc.exhausted {
					return nil, dbaccess.ErrExhausted
				}
				return release, nil
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || got != nil {
					t.Fatalf("admit = %v, %v; want %v", got != nil, err, tc.wantErr)
				}
			} else if err != nil || got == nil {
				t.Fatalf("admit = %v; want a slot", err)
			}
			if tc.wantCalls != 0 && calls != tc.wantCalls {
				t.Fatalf("admission attempts = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestFormatFactsJob(t *testing.T) {
	out := formatFactsJob(&facts.Job{ID: "J1", Model: "Sale", Status: facts.JobRunning, Checkpoint: "SAL-00010", Processed: 10, StartSequence: 41})
	for _, want := range []string{"Job: J1", "Model: Sale", "Status: running", `Checkpoint: "SAL-00010"`, "Processed: 10", "Start sequence: 41"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFactsCommandsRegistered(t *testing.T) {
	want := map[string]bool{"rebuild": false, "rebuild-status": false, "rebuild-cancel": false}
	for _, c := range factsCmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("facts command %q not registered", name)
		}
	}
}
