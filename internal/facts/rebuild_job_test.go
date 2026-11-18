package facts

import "testing"

// TestJobStatusMoves checks every (from, to) pair of job states against the allowed moves.
func TestJobStatusMoves(t *testing.T) {
	allowed := map[[2]JobStatus]bool{
		{JobQueued, JobRunning}:    true,
		{JobRunning, JobDone}:      true,
		{JobRunning, JobFailed}:    true,
		{JobRunning, JobCancelled}: true,
	}
	for _, from := range allJobStatuses {
		for _, to := range allJobStatuses {
			want := allowed[[2]JobStatus{from, to}]
			if got := from.CanMoveTo(to); got != want {
				t.Errorf("%s -> %s: CanMoveTo = %v, want %v", from, to, got, want)
			}
		}
	}
}

// TestSourcesOf checks that the guarded UPDATE for each target lists exactly its legal sources.
func TestSourcesOf(t *testing.T) {
	tests := []struct {
		to   JobStatus
		want []JobStatus
	}{
		{JobQueued, nil},
		{JobRunning, []JobStatus{JobQueued}},
		{JobDone, []JobStatus{JobRunning}},
		{JobFailed, []JobStatus{JobRunning}},
		{JobCancelled, []JobStatus{JobRunning}},
	}
	for _, tc := range tests {
		got := sourcesOf(tc.to)
		if len(got) != len(tc.want) {
			t.Fatalf("sourcesOf(%s) = %v, want %v", tc.to, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("sourcesOf(%s) = %v, want %v", tc.to, got, tc.want)
			}
		}
	}
}
