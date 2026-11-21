package facts

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// EnqueueGenerated queues one whole-model rebuild for each generated fact model that has no backfill queued
// or running and has not finished one, so existing records reach the new fact table. The drift checker runs
// queued jobs. It returns how many jobs it queued.
func EnqueueGenerated(ctx context.Context, database *sql.DB, dialect db.Dialect, reg *doctype.Registry) (int, error) {
	var models []string
	for _, dt := range reg.All() {
		if dt.Facts != nil && dt.Facts.Generated {
			models = append(models, dt.Name)
		}
	}
	slices.Sort(models)
	store := &JobStore{DB: database, Dialect: dialect}
	queued := 0
	for _, model := range models {
		job, err := store.EnqueueOnce(ctx, model)
		if err != nil {
			return queued, fmt.Errorf("facts: queue generated rebuild of %s: %w", model, err)
		}
		if job != nil {
			queued++
		}
	}
	return queued, nil
}
