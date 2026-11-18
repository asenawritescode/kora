package engine

import (
	"context"
	"errors"
	"fmt"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/facts"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

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
	if err := dispatcher.RegisterTransactionalForTypes(facts.ConsumerName, types, builder.Consume); err != nil {
		return fmt.Errorf("register fact events: %w", err)
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
