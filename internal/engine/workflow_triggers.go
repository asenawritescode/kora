package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/internal/tenantruntime"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/workflow"
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
	if err := dispatcher.RegisterForTypes("workflow-record-triggers", []string{"record.created", "record.updated"}, func(ctx context.Context, event contract.EventEnvelope) error {
		release, err := services.WorkAdmission.Acquire(ctx)
		if err != nil {
			return err
		}
		defer release()
		_, err = instances.StartFromRecordEvent(ctx, tenant, loaded.Name, event)
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
			slog.Warn("workflow trigger dispatch failed", "error_type", fmt.Sprintf("%T", err))
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
