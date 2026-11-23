package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/sweep"
	"github.com/asenawritescode/kora/internal/tenantruntime"
	"github.com/asenawritescode/kora/internal/workflow"
)

// recordTriggerInstances is the instance service record triggers need, or nil when workflows are not composed.
func recordTriggerInstances(definitions *workflow.Service, instances *workflow.InstanceService) *workflow.InstanceService {
	if definitions == nil {
		return nil
	}
	return instances
}

// registerRecordConsumers registers the tenant's transactional consumers of record events.
func registerRecordConsumers(config TenantServiceConfig, loaded *knet.LoadedSite, services *tenantruntime.Services, instances *workflow.InstanceService, dialect kdb.Dialect) error {
	if instances != nil {
		if err := registerWorkflowTriggerConsumer(config, loaded, services, instances, dialect); err != nil {
			return fmt.Errorf("register record-triggered workflow consumer for tenant %s: %w", loaded.Name, err)
		}
	}
	if err := registerFactConsumer(config, loaded, services, dialect); err != nil {
		return fmt.Errorf("register fact consumer for tenant %s: %w", loaded.Name, err)
	}
	return nil
}

func registerWorkflowTriggerConsumer(config TenantServiceConfig, loaded *knet.LoadedSite, services *tenantruntime.Services, instances *workflow.InstanceService, dialect kdb.Dialect) error {
	if loaded == nil || loaded.DB == nil || services == nil || instances == nil || services.WorkAdmission == nil {
		return errors.New("record-triggered workflows require a tenant database, instance service and work admission")
	}
	tenant, err := contract.ParseTenantID(loaded.SiteID)
	if err != nil {
		return fmt.Errorf("parse workflow trigger tenant: %w", err)
	}
	dispatcher := outbox.NewDispatcher(loaded.DB, dialect)
	background := services.WorkAdmission.Background()
	dispatcher.WorkAdmission = background
	wake := services.OutboxWake.Subscribe()
	if err := registerRecordTriggerConsumer(dispatcher, func(ctx context.Context, trigger contract.EventEnvelope) error {
		release, err := background.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
		_, err = instances.StartFromRecordEvent(ctx, tenant, loaded.Name, trigger)
		return err
	}); err != nil {
		return fmt.Errorf("register workflow trigger events: %w", err)
	}
	return registerTenantWorkerStart(config, func() error {
		if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
			RunWorkflowTriggerDispatcher(ctx, dispatcher, wake)
		}) {
			return errors.New("tenant runtime is stopping")
		}
		return nil
	})
}

// RunWorkflowTriggerDispatcher dispatches outbox events to record-triggered
// workflow starts. A full batch drains at once; otherwise it waits for a wake
// or the sweep.
func RunWorkflowTriggerDispatcher(ctx context.Context, dispatcher *outbox.Dispatcher, wake <-chan struct{}) {
	if ctx == nil || dispatcher == nil {
		return
	}
	sweep.Loop(ctx, sweep.New(backgroundSweep, wake), func(passCtx context.Context) bool {
		workCtx, cancel := context.WithTimeout(passCtx, 30*time.Second)
		processed, err := dispatcher.Run(workCtx, dispatcherBatch)
		cancel()
		if err != nil && passCtx.Err() == nil && !dbaccess.IsBackgroundBusy(err) {
			// Outbox delivery is retried by leaving the event unreceipted. Avoid
			// logging record contents because they may contain user data.
			var domainErr *contract.Error
			if errors.As(err, &domainErr) && domainErr.Type == contract.CodeResourceExhausted {
				slog.Warn("workflow trigger dispatch stopped at its causal bound", "error_code", domainErr.Type)
			} else {
				slog.Warn("workflow trigger dispatch failed", "error_type", fmt.Sprintf("%T", err))
			}
		}
		return err == nil && processed == dispatcherBatch
	})
}

// registerRecordTriggerConsumer delivers record change events to start as
// record trigger envelopes.
func registerRecordTriggerConsumer(dispatcher *outbox.Dispatcher, start func(context.Context, contract.EventEnvelope) error) error {
	return dispatcher.RegisterForPatterns("workflow-record-triggers", workflow.RecordTriggerEventPatterns, func(ctx context.Context, event contract.EventEnvelope) error {
		trigger, ok := workflow.RecordTriggerFromChangeEvent(event)
		if !ok {
			return nil
		}
		return start(ctx, trigger)
	})
}
