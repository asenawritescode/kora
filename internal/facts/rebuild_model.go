package facts

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// RebuildRequest names the site and model a rebuild covers, with the handles it runs on.
type RebuildRequest struct {
	DB       *sql.DB
	Dialect  db.Dialect
	Registry *doctype.Registry
	Site     string
	Model    string
}

// RebuildModel enqueues a rebuild of req.Model, claims the oldest unfinished job of that model, and runs
// it to the end. An earlier job left queued or expired is resumed first, so rerunning the command resumes.
func RebuildModel(ctx context.Context, req RebuildRequest) (*Job, error) {
	database, dialect, model := req.DB, req.Dialect, req.Model
	if dt := req.Registry.Get(model); dt == nil || dt.Facts == nil {
		return nil, fmt.Errorf("doctype %q has no fact model on site %q", model, req.Site)
	}
	store := &JobStore{DB: database, Dialect: dialect}
	queued, err := store.Enqueue(ctx, model, Window{})
	if err != nil {
		return nil, err
	}
	claimed, err := store.Claim(ctx, model, contract.NewEventID())
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return nil, fmt.Errorf("rebuild job %s is held by another worker", queued.ID)
	}
	rebuilder := &Rebuilder{DB: database, Dialect: dialect, Registry: req.Registry, Site: req.Site, Store: store}
	if err = rebuilder.Run(ctx, claimed); err != nil {
		return nil, fmt.Errorf("rebuild job %s: %w", claimed.ID, err)
	}
	return store.Get(ctx, claimed.ID)
}
