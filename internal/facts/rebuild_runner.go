package facts

import (
	"context"
	"database/sql"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// rebuildRunnerEvery is the pause between passes of a RebuildRunner.
const rebuildRunnerEvery = 15 * time.Second

// RebuildRunner runs the queued rebuild jobs of a tenant's fact models on its own schedule, so a job queued by
// a sync or a config change starts within one pass, without waiting for the drift check. Each job is taken by
// its own fenced claim, so replicas and the drift check can run the same loop: a job runs on one of them.
type RebuildRunner struct {
	DB       *sql.DB
	Dialect  db.Dialect
	Registry *doctype.Registry
	Site     string
	Owner    string        // lease owner; unique per process
	Every    time.Duration // pause between passes; zero means rebuildRunnerEvery
}

// Drain runs every queued job of every fact model once and reports how many it ran.
func (r *RebuildRunner) Drain(ctx context.Context) (int, error) {
	store := &JobStore{DB: r.DB, Dialect: r.Dialect}
	rebuilder := &Rebuilder{DB: r.DB, Dialect: r.Dialect, Registry: r.Registry, Site: r.Site, Store: store}
	return drainRebuildJobs(ctx, store, rebuilder, r.Owner, factModels(r.Registry))
}

// Run drains until ctx ends. A failed pass is logged; its jobs stay queued or resume on the next pass.
func (r *RebuildRunner) Run(ctx context.Context) {
	every := r.Every
	if every == 0 {
		every = rebuildRunnerEvery
	}
	for {
		if _, err := r.Drain(ctx); err != nil && ctx.Err() == nil {
			slog.Error("run fact rebuild jobs", "site", r.Site, "error", err)
		}
		pause := every + rand.N(every/2) //nolint:gosec // jitter spreads passes; no security use
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// drainRebuildJobs claims and runs the queued jobs of each model, oldest first, until none is claimable.
// A job that fails stops the drain and returns its error; the job itself records the failure.
func drainRebuildJobs(ctx context.Context, store *JobStore, rebuilder *Rebuilder, owner string, models []*doctype.DocType) (int, error) {
	ran := 0
	for _, dt := range models {
		for {
			job, err := store.Claim(ctx, dt.Name, owner)
			if err != nil {
				return ran, err
			}
			if job == nil {
				break
			}
			if err := rebuilder.Run(ctx, job); err != nil {
				return ran, err
			}
			ran++
		}
	}
	return ran, nil
}

// factModels lists the doctypes with a fact model, sorted by name.
func factModels(reg *doctype.Registry) []*doctype.DocType {
	var out []*doctype.DocType
	for _, dt := range reg.All() {
		if dt.Facts != nil {
			out = append(out, dt)
		}
	}
	slices.SortFunc(out, func(a, b *doctype.DocType) int { return strings.Compare(a.Name, b.Name) })
	return out
}
