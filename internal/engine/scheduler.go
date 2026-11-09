package engine

import (
	"context"
	"database/sql"
	"errors"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/email"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/scheduler"
)

// SchedulerOptions describes explicitly supplied scheduler definitions and
// the tenant-scoped dependencies required to execute them.
type SchedulerOptions struct {
	Jobs         []*scheduler.JobConfig
	Database     *sql.DB
	Registry     *doctype.Registry
	Transactions *orm.TxManager
	Mailer       *email.Sender
}

// StartScheduler starts the scheduler for standalone callers.
func StartScheduler(ctx context.Context, options SchedulerOptions) (*scheduler.Scheduler, error) {
	worker, err := PrepareScheduler(options)
	if err != nil || worker == nil {
		return worker, err
	}
	worker.StartContext(ctx)
	return worker, nil
}

// PrepareScheduler registers explicitly supplied jobs without starting
// goroutines. The caller owns the worker activation boundary.
func PrepareScheduler(options SchedulerOptions) (*scheduler.Scheduler, error) {
	if len(options.Jobs) == 0 {
		return nil, nil
	}
	if options.Database == nil || options.Registry == nil || options.Transactions == nil {
		return nil, errors.New("scheduler database, registry, and transaction manager are required when jobs are configured")
	}
	mailer := options.Mailer
	if mailer == nil {
		mailer = email.NewSender(&email.Config{From: "kora@localhost"})
	}
	worker := scheduler.New(options.Database, options.Registry, options.Transactions, mailer)
	for _, job := range options.Jobs {
		worker.RegisterJob(job)
	}
	return worker, nil
}
