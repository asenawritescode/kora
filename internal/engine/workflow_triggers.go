package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
// record trigger envelopes. Legacy record.* types drain events written before
// each mutation had a single change event.
func registerRecordTriggerConsumer(dispatcher *outbox.Dispatcher, start func(context.Context, contract.EventEnvelope) error) error {
	return dispatcher.RegisterForPatterns("workflow-record-triggers", []string{
		"kora.%.after!_insert", "kora.%.after!_save", "record.created", "record.updated",
	}, func(ctx context.Context, event contract.EventEnvelope) error {
		trigger, ok := recordTriggerEvent(event)
		if !ok {
			return nil
		}
		return start(ctx, trigger)
	})
}

// recordTriggerEvent projects a kernel change event onto the record trigger
// contract. Writes without record identity, and updates that changed no
// field, start no workflow.
func recordTriggerEvent(event contract.EventEnvelope) (contract.EventEnvelope, bool) {
	if event.Type == "record.created" || event.Type == "record.updated" {
		return event, true
	}
	kind := ""
	switch {
	case strings.HasSuffix(event.Type, ".after_insert"):
		kind = "record.created"
	case strings.HasSuffix(event.Type, ".after_save"):
		kind = "record.updated"
	default:
		return contract.EventEnvelope{}, false
	}
	var payload contract.RecordChangeEventData
	if err := json.Unmarshal(event.Data, &payload); err != nil || payload.Table == "" || payload.RecordID == "" || payload.Revision == "" {
		return contract.EventEnvelope{}, false
	}
	if kind == "record.updated" && len(payload.ChangedFields) == 0 {
		return contract.EventEnvelope{}, false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return contract.EventEnvelope{}, false
	}
	trigger := event
	trigger.Type = kind
	trigger.Data = data
	return trigger, true
}
