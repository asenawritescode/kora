package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenantruntime"
	"github.com/asenawritescode/kora/internal/workflow"
)

func registerWorkflowTriggerConsumer(config TenantServiceConfig, loaded *knet.LoadedSite, services *tenantruntime.Services, instances *workflow.InstanceService, dialect kdb.Dialect) error {
	if loaded == nil || loaded.DB == nil || services == nil || instances == nil || services.WorkAdmission == nil {
		return errors.New("record-triggered workflows require a tenant database, instance service and work admission")
	}
	tenant, err := contract.ParseTenantID(loaded.SiteID)
	if err != nil {
		return fmt.Errorf("parse workflow trigger tenant: %w", err)
	}
	dispatcher := outbox.NewDispatcher(loaded.DB, dialect)
	dispatcher.WorkAdmission = services.WorkAdmission
	if err := registerRecordTriggerConsumer(dispatcher, func(ctx context.Context, trigger contract.EventEnvelope) error {
		release, err := services.WorkAdmission.Acquire(ctx)
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
			RunWorkflowTriggerDispatcher(ctx, dispatcher)
		}) {
			return errors.New("tenant runtime is stopping")
		}
		return nil
	})
}

func RunWorkflowTriggerDispatcher(ctx context.Context, dispatcher *outbox.Dispatcher) {
	if ctx == nil || dispatcher == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		processed, err := dispatcher.Run(workCtx, 25)
		cancel()
		if err != nil && ctx.Err() == nil {
			// Outbox delivery is retried by leaving the event unreceipted. Avoid
			// logging record contents because they may contain user data.
			var domainErr *contract.Error
			if errors.As(err, &domainErr) && domainErr.Type == contract.CodeResourceExhausted {
				slog.Warn("workflow trigger dispatch stopped at its causal bound", "error_code", domainErr.Type)
			} else {
				slog.Warn("workflow trigger dispatch failed", "error_type", fmt.Sprintf("%T", err))
			}
		}
		if processed > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
