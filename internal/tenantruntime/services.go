// Package tenantruntime contains the typed, immutable service references
// captured by one tenant runtime generation.
package tenantruntime

import (
	"context"
	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
	"github.com/asenawritescode/kora/scheduler"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/storage"
	"github.com/asenawritescode/kora/webhook"
	"github.com/asenawritescode/kora/workflow"
)

// Services is the immutable service snapshot owned by a single tenant runtime.
// Constructors and lifecycle ownership stay in Engine composition.
type Services struct {
	ModuleBindings                 *plugin.BindingPlan
	ResolveModuleBindings          func(contract.BusinessScope) (plugin.BindingPlan, error)
	ResolveCandidateModuleBindings func(contract.BusinessScope, []contract.ModuleDescriptor, []contract.ProviderSelection, uint64) (plugin.BindingPlan, error)
	PublishedProjections           *analytics.PublishedProjectionStore
	WorkAdmission                  *dbaccess.Admission
	RealtimeAdmission              interface {
		Acquire(context.Context) (func(), error)
	}
	UploadAdmission interface {
		Acquire(context.Context, uint64) (func(), error)
	}
	EventBus                        eventstream.Bus
	Mailer                          *email.Sender
	AnalyticsRelay                  *analytics.CloudRelay
	AnalyticsRebuild                *analytics.RebuildRunner
	Realtime                        *natsprovider.Provider
	ScriptStore                     *script.Store
	AsyncHookInvocations            script.AsyncHookInvocationStore
	SecretStore                     *secret.Store
	WebhookWorker                   *webhook.Worker
	Outbox                          outbox.Writer
	OutboxBacklog                   *outbox.BacklogMetrics
	Storage                         storage.Backend
	Requests                        *requests.Service
	ApprovalApplier                 ApprovalApplier
	WorkflowDefinitions             *workflow.Service
	WorkflowInstances               *workflow.InstanceService
	WorkflowStatusPolicies          workflow.StatusPolicyRepository
	WorkflowApprovalPoliciesByTable map[string][]string
	Scheduler                       *scheduler.Scheduler
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
