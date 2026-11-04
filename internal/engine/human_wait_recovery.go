package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	knet "github.com/asenawritescode/kora/net"
	requestsql "github.com/asenawritescode/kora/requests/sqlstore"
	"github.com/asenawritescode/kora/workflow"
	workflowsql "github.com/asenawritescode/kora/workflow/sqlstore"
)

// StartHumanWaitRecoveryWorker starts the tenant-owned sweeper that expires
// requests and resumes workflows waiting on terminal request or timer states.
// The runtime generation owns cancellation and joins it before DB closure.
func StartHumanWaitRecoveryWorker(runtime *knet.LoadedSite, processCtx context.Context) error {
	if runtime == nil || runtime.DB == nil {
		return fmt.Errorf("tenant runtime and database are required")
	}
	tenant, err := contract.ParseTenantID(runtime.SiteID)
	if err != nil {
		return fmt.Errorf("invalid stable tenant identity: %w", err)
	}
	executionStore, err := workflowsql.NewExecutionStore(runtime.DB, kdb.Resolve(runtime.DBType))
	if err != nil {
		return fmt.Errorf("construct durable workflow execution store: %w", err)
	}
	requestStore, err := requestsql.New(runtime.DB, kdb.Resolve(runtime.DBType))
	if err != nil {
		return fmt.Errorf("construct durable request store: %w", err)
	}
	recoveryStore := tenantHumanWaitRecoveryStore{requests: requestStore, workflows: executionStore}
	services := runtime.RuntimeServiceSnapshot()
	worker, err := workflow.NewHumanWaitRecoveryWorker(recoveryStore, tenant, 25, 15*time.Second, func(err error) {
		slog.Warn("workflow human request recovery pass failed", "tenant_id", tenant, "error", err)
	}, services.WorkAdmission)
	if err != nil {
		return fmt.Errorf("construct human request recovery worker: %w", err)
	}
	if !runtime.StartWorker(processCtx, worker.Run) {
		return fmt.Errorf("tenant runtime generation %d is stopping", runtime.EnsureRuntimeGeneration())
	}
	return nil
}

type tenantHumanWaitRecoveryStore struct {
	requests interface {
		ExpireDue(context.Context, contract.TenantID, uint16) (uint16, error)
	}
	workflows interface {
		ResumeTerminalHumanWaits(context.Context, contract.TenantID, uint16) (uint16, error)
		ResumeDueTimedWaits(context.Context, contract.TenantID, uint16) (uint16, error)
	}
}

func (s tenantHumanWaitRecoveryStore) ExpireDue(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	return s.requests.ExpireDue(ctx, tenant, limit)
}

func (s tenantHumanWaitRecoveryStore) ResumeTerminalHumanWaits(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	return s.workflows.ResumeTerminalHumanWaits(ctx, tenant, limit)
}

func (s tenantHumanWaitRecoveryStore) ResumeDueTimedWaits(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	return s.workflows.ResumeDueTimedWaits(ctx, tenant, limit)
}
