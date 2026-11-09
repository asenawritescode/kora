// Package tenantruntime contains the typed, immutable service references
// captured by one tenant runtime generation.
package tenantruntime

import (
	"context"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/email"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/natsprovider"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/print"
	"github.com/asenawritescode/kora/internal/query"
	"github.com/asenawritescode/kora/internal/recon"
	"github.com/asenawritescode/kora/internal/requests"
	"github.com/asenawritescode/kora/internal/scheduler"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/secret"
	"github.com/asenawritescode/kora/internal/storage"
	"github.com/asenawritescode/kora/internal/webhook"
	"github.com/asenawritescode/kora/internal/workflow"
)

// Services is the immutable service snapshot owned by a single tenant runtime.
// Constructors and lifecycle ownership stay in Engine composition.
type Services struct {
	// KernelCommands is the tenant generation's command registry: process-wide
	// definitions plus those carried by the tenant's active configuration.
	KernelCommands *kernel.CommandRegistry
	// PrintFormats are the tenant's configured print formats keyed by name.
	PrintFormats map[string]*print.Format
	// ConfigQueries are the queries carried by the tenant's active configuration.
	ConfigQueries         []*query.Definition
	CommandSchemas        kernel.CommandSchemaRegistry
	ModuleBindings        *plugin.BindingPlan
	ResolveModuleBindings func(contract.BusinessScope) (plugin.BindingPlan, error)
	// ResolveCandidateModuleBindings preflights a candidate module set. A
	// non-nil candidate configuration supplies the commands and queries that
	// activation would install; nil keeps the active configuration's.
	ResolveCandidateModuleBindings func(contract.BusinessScope, []contract.InstalledPackage, []contract.ModuleDescriptor, *doctype.ConfigSnapshot, []contract.ProviderSelection, uint64) (plugin.BindingPlan, error)
	LoadModuleSnapshot             func() (contract.ModuleSnapshot, error)
	ExecutePublishedOperation      func(context.Context, policy.Execution, contract.BusinessScope, contract.ModuleID, contract.PublishedOperation, contract.SchemaRef, contract.CommandMeta, []byte) (contract.OperationReceipt, error)
	LookupPublishedOperation       func(context.Context, policy.Execution, contract.BusinessScope, contract.ModuleID, contract.PublishedOperation, contract.IdempotencyKey) (contract.OperationReceipt, bool, error)
	LookupPublishedOperationByID   func(context.Context, policy.Execution, contract.BusinessScope, contract.OperationID) (contract.OperationReceipt, bool, error)
	PublishedProjections           *analytics.PublishedProjectionStore
	BusinessReconciliation         *recon.BusinessReconciliationService
	WorkAdmission                  *dbaccess.Admission
	RealtimeAdmission              interface {
		Acquire(context.Context) (func(), error)
	}
	UploadAdmission interface {
		Acquire(context.Context, uint64) (func(), error)
	}
	EventBus                  eventstream.Bus
	Mailer                    *email.Sender
	AnalyticsRelay            *analytics.CloudRelay
	AnalyticsRebuild          *analytics.RebuildRunner
	Realtime                  *natsprovider.Provider
	ScriptStore               *script.Store
	AsyncHookInvocations      script.AsyncHookInvocationStore
	SecretStore               *secret.Store
	WebhookWorker             *webhook.Worker
	Outbox                    outbox.Writer
	OutboxBacklog             *outbox.BacklogMetrics
	Storage                   storage.Backend
	Requests                  *requests.Service
	ApprovalApplier           ApprovalApplier
	WorkflowDefinitions       *workflow.Service
	WorkflowInstances         *workflow.InstanceService
	WorkflowStatusPolicies    workflow.StatusPolicyRepository
	WorkflowOpenWorkInspector interface {
		InspectOpenWork(context.Context, contract.TenantID, contract.ModuleID, uint16) ([]contract.ModuleBlocker, error)
	}
	ModuleOpenWorkInspectors        []ModuleOpenWorkInspector
	WorkflowApprovalPoliciesByTable map[string][]string
	Scheduler                       *scheduler.Scheduler
}

// ModuleOpenWorkInspector binds a module owner to its lifecycle inspection
// implementation in the immutable tenant runtime service snapshot.
type ModuleOpenWorkInspector struct {
	ModuleID  contract.ModuleID
	Inspector contract.OpenWorkInspector
}

// EnqueueCloudProductEvent submits best-effort telemetry to the tenant-owned
// relay. A missing relay or a full queue drops telemetry without affecting the
// caller's business operation.
func (s Services) EnqueueCloudProductEvent(event analytics.CloudProductEventDTO, dedupeKey string) bool {
	return s.AnalyticsRelay != nil && s.AnalyticsRelay.EnqueueProductEvent(event, dedupeKey)
}

// ApprovalApplier is the only supported runtime path for applying a governed
// approval to a protected local domain transition.
type ApprovalApplier interface {
	ApproveAndApply(context.Context, policy.Execution, contract.RequestID, contract.RespondRequest) (contract.ResponseReceipt, error)
}
