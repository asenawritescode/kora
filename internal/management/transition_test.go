package management

import (
	"slices"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestOperationTransitionTableCoversEveryStatePair(t *testing.T) {
	const (
		requested = contract.OperationRequested
		running   = contract.OperationRunning
		succeeded = contract.OperationSucceeded
		failed    = contract.OperationFailed
		recovery  = contract.OperationRecoveryRequired
	)
	states := []contract.OperationState{requested, running, succeeded, failed, recovery}
	moves := []Move{MoveClaimDue, MoveRenew, MoveRelease, MoveFinish, MoveRetry}
	// want lists the moves that perform each allowed transition; every pair
	// not listed must be refused by every move.
	want := map[transition][]Move{
		{requested, running}:  {MoveClaimDue},
		{running, running}:    {MoveRenew, MoveClaimDue},
		{running, requested}:  {MoveRelease},
		{running, succeeded}:  {MoveFinish},
		{running, failed}:     {MoveFinish},
		{running, recovery}:   {MoveFinish},
		{recovery, requested}: {MoveRetry},
	}
	for _, from := range states {
		for _, to := range states {
			pair := transition{from, to}
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				if got := CanTransition(from, to); got != (len(want[pair]) > 0) {
					t.Fatalf("CanTransition = %v, want %v", got, len(want[pair]) > 0)
				}
				for _, move := range moves {
					if got, expect := move.Allows(from, to), slices.Contains(want[pair], move); got != expect {
						t.Errorf("%s.Allows = %v, want %v", move, got, expect)
					}
				}
			})
		}
	}
	if len(allowed) != len(want) {
		t.Fatalf("transition table has %d entries, want %d", len(allowed), len(want))
	}
}
