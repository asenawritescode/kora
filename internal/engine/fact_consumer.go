package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/facts"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

// factModelSyncRetry is the pause between failed fact model syncs.
const factModelSyncRetry = 30 * time.Second

// backfillGenerated queues the backfill of generated models that have none open or finished. It runs after
// the sync, so a model the sync just queued already has its job and is left alone. A failure only delays it.
func backfillGenerated(ctx context.Context, tenant string, modelSync *facts.ModelSync) {
	if _, err := facts.EnqueueGenerated(ctx, modelSync.DB, modelSync.Dialect, modelSync.Registry); err != nil {
		slog.Warn("queue generated fact rebuilds", "tenant", tenant, "error", err)
	}
}

// syncFactModels syncs the fact models until it succeeds or ctx ends. The fact consumer starts only after
// this returns true, so no event reaches a table that is about to be recreated.
func syncFactModels(ctx context.Context, tenant string, modelSync *facts.ModelSync) bool {
	for {
		err := modelSync.Sync(ctx)
		if err == nil {
			backfillGenerated(ctx, tenant, modelSync)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		slog.Error("sync fact models; retrying", "tenant", tenant, "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(factModelSyncRetry):
		}
	}
}

// registerFactConsumer keeps fact tables current from record events of doctypes with a fact model.
func registerFactConsumer(config TenantServiceConfig, loaded *knet.LoadedSite, services *tenantruntime.Services, dialect kdb.Dialect) error {
	types := facts.EventTypes(loaded.Registry)
	if len(types) == 0 {
		return nil
	}
	if loaded.DB == nil || services.WorkAdmission == nil {
		return errors.New("fact tables require a tenant database and work admission")
	}
	dispatcher := outbox.NewDispatcher(loaded.DB, dialect)
	dispatcher.WorkAdmission = services.WorkAdmission
	builder := &facts.Builder{DB: loaded.DB, Dialect: dialect, Registry: loaded.Registry}
	refresher := &facts.Refresher{DB: loaded.DB, Dialect: dialect, Registry: loaded.Registry, Admission: services.WorkAdmission, Owner: contract.NewEventID()}
	driftCounter, err := facts.NewDriftCounter()
	if err != nil {
		return fmt.Errorf("register fact drift metric: %w", err)
	}
	drift := &facts.DriftChecker{DB: loaded.DB, Dialect: dialect, Registry: loaded.Registry, Site: loaded.Name,
		Admission: services.WorkAdmission, Owner: contract.NewEventID(), Drifted: driftCounter}
	if err := dispatcher.RegisterTransactionalForTypes(facts.ConsumerName, types, builder.Consume); err != nil {
		return fmt.Errorf("register fact events: %w", err)
	}
	modelSync := &facts.ModelSync{DB: loaded.DB, Dialect: dialect, Registry: loaded.Registry, Owner: contract.NewEventID()}
	return registerTenantWorkerStart(config, func() error {
		// The sync, the event dispatcher and the rebuild runner share one worker: the runner starts only after
		// the sync, and the dispatcher only after the runner's first pass has been started.
		runner := &facts.RebuildRunner{DB: loaded.DB, Dialect: dialect, Registry: loaded.Registry, Site: loaded.Name, Owner: contract.NewEventID()}
		if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
			if !syncFactModels(ctx, loaded.Name, modelSync) {
				return
			}
			dispatched := make(chan struct{})
			go func() {
				defer close(dispatched)
				RunWorkflowTriggerDispatcher(ctx, dispatcher)
			}()
			runner.Run(ctx)
			<-dispatched
		}) {
			return errors.New("tenant runtime is stopping")
		}
		if !loaded.StartWorker(config.ProcessContext, refresher.Run) {
			return errors.New("tenant runtime is stopping")
		}
		if !loaded.StartWorker(config.ProcessContext, drift.Run) {
			return errors.New("tenant runtime is stopping")
		}
		return nil
	})
}
