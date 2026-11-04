//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"testing"

	"github.com/asenawritescode/kora/db"
)

func claimTwoRebuildRunners(t *testing.T, database *sql.DB, dialect db.Dialect, site string) (*RebuildJob, string) {
	t.Helper()
	start := make(chan struct{})
	type result struct {
		owner string
		job   *RebuildJob
		err   error
	}
	results := make(chan result, 2)
	for _, owner := range []string{"runner-a", "runner-b"} {
		go func(owner string) {
			<-start
			job, err := ClaimNextRebuildJob(context.Background(), database, dialect, site, owner)
			results <- result{owner: owner, job: job, err: err}
		}(owner)
	}
	close(start)
	var winner result
	for range 2 {
		candidate := <-results
		if candidate.err != nil {
			t.Fatalf("concurrent claim by %s: %v", candidate.owner, candidate.err)
		}
		if candidate.job != nil {
			if winner.job != nil {
				t.Fatal("both concurrent rebuild runners claimed the same job")
			}
			winner = candidate
		}
	}
	if winner.job == nil || winner.job.FencingToken != 1 {
		t.Fatalf("concurrent claim winner = %#v, want fence 1", winner)
	}
	if winner.owner == "runner-a" {
		return winner.job, "runner-b"
	}
	return winner.job, "runner-a"
}
