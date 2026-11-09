// Package process composes local request decisions with protected domain
// operations in one tenant transaction. It owns transaction coordination, not
// request policy or business invariants.
package process

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
)

// TenantTransaction opens one tenant transaction and returns its release
// callback. Implementations acquire tenant active-work admission before
// opening SQL and release it only after commit or rollback.
type TenantTransaction func(context.Context, contract.TenantID) (database *sql.DB, transaction *sql.Tx, release func(), err error)

// NewTenantTransactionOpener binds the coordinator to one stable tenant DB
// and the same bounded active-work admission used by its HTTP/request path.
// An existing request admission is reused; non-HTTP callers acquire a permit
// here and hold it until the coordinator commits or rolls back.
func NewTenantTransactionOpener(tenant contract.TenantID, database *sql.DB, admission *dbaccess.Admission) (TenantTransaction, error) {
	if tenant == "" || database == nil || admission == nil {
		return nil, fmt.Errorf("tenant transaction opener requires tenant, database and active-work admission")
	}
	return func(ctx context.Context, requestedTenant contract.TenantID) (*sql.DB, *sql.Tx, func(), error) {
		if requestedTenant != tenant {
			return nil, nil, nil, contract.NewError(contract.CodePermissionDenied, "tenant transaction requested for a different tenant")
		}
		tx, release, err := dbaccess.BeginAdmittedTx(ctx, database, admission)
		if err != nil {
			return nil, nil, nil, err
		}
		return database, tx, release, nil
	}, nil
}

// WorkflowApproval maps a server-owned governed policy to a named document
// workflow action. Request bodies cannot choose this mapping or command.
type WorkflowApproval struct {
	SubjectKind  string
	Action       string
	RejectAction string
}

type Coordinator struct {
	requests  *requests.Service
	engine    *kernel.Kernel
	registry  *doctype.Registry
	begin     TenantTransaction
	workflows map[string]WorkflowApproval
}

func NewCoordinator(requestService *requests.Service, engine *kernel.Kernel, registry *doctype.Registry, begin TenantTransaction, workflows map[string]WorkflowApproval) (*Coordinator, error) {
	if requestService == nil || engine == nil || registry == nil || begin == nil || len(workflows) == 0 {
		return nil, fmt.Errorf("approval coordinator requires request service, kernel, registry, transaction opener and workflow policy map")
	}
	owned := make(map[string]WorkflowApproval, len(workflows))
	for policyID, action := range workflows {
		if policyID == "" || strings.TrimSpace(action.SubjectKind) == "" || strings.TrimSpace(action.Action) == "" {
			return nil, fmt.Errorf("approval workflow policy, subject kind and action must be non-empty")
		}
		owned[policyID] = action
	}
	return &Coordinator{requests: requestService, engine: engine, registry: registry, begin: begin, workflows: owned}, nil
}

// ApproveAndApply records an authorized approval response and applies its
// registered workflow transition atomically when that response resolves the
// request as approved or rejected. The request service performs current
// response authorization before Begin is called. A replay returns the original
// receipt without opening a transaction or rerunning the effect.
func (c *Coordinator) ApproveAndApply(ctx context.Context, exec policy.Execution, requestID contract.RequestID, command contract.RespondRequest) (contract.ResponseReceipt, error) {
	if c == nil || c.requests == nil || c.engine == nil || c.registry == nil || c.begin == nil {
		return contract.ResponseReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "approval coordinator is unavailable")
	}
	prepared, err := c.requests.PrepareResponse(ctx, exec, requestID, command)
	if err != nil {
		return contract.ResponseReceipt{}, err
	}
	if receipt, replay := prepared.ReplayReceipt(); replay {
		return receipt, nil
	}
	requestContext, ok := prepared.Context()
	if !ok || requestContext.Tenant != exec.Tenant() || requestContext.Kind != contract.RequestApproval || requestContext.Subject == nil {
		return contract.ResponseReceipt{}, contract.NewError(contract.CodePermissionDenied, "prepared request is not a subject-bound governed approval")
	}
	workflow, ok := c.workflows[requestContext.PolicyID]
	if !ok {
		return contract.ResponseReceipt{}, contract.NewError(contract.CodePermissionDenied, "approval policy has no registered local domain action")
	}
	if requestContext.Subject.Kind != workflow.SubjectKind {
		return contract.ResponseReceipt{}, contract.NewError(contract.CodePermissionDenied, "approval policy is not registered for this record type")
	}
	registeredWorkflow := WorkflowApproval{}
	switch command.Response {
	case contract.ResponseApprove:
		registeredWorkflow.Action = workflow.Action
	case contract.ResponseReject:
		registeredWorkflow.Action = workflow.RejectAction
	default:
		return contract.ResponseReceipt{}, contract.NewError(contract.CodeValidationFailed, "approval response has no registered local domain action")
	}
	if registeredWorkflow.Action == "" {
		return contract.ResponseReceipt{}, contract.NewError(contract.CodePermissionDenied, "approval response has no registered local domain action")
	}
	operation, err := workflowOperation(exec, requestContext, registeredWorkflow, command.Meta.IdempotencyKey)
	if err != nil {
		return contract.ResponseReceipt{}, err
	}
	database, tx, release, err := c.begin(ctx, exec.Tenant())
	if err != nil {
		if release != nil {
			release()
		}
		return contract.ResponseReceipt{}, err
	}
	if database == nil || tx == nil {
		if release != nil {
			release()
		}
		return contract.ResponseReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant transaction opener returned no transaction")
	}
	defer func() {
		_ = tx.Rollback()
		if release != nil {
			release()
		}
	}()
	receipt, err := prepared.CommitInTx(ctx, tx)
	if err != nil {
		return contract.ResponseReceipt{}, err
	}
	shouldApplyDomainAction := receipt.Status == contract.RequestResolved && receipt.Outcome != nil &&
		((*receipt.Outcome == contract.OutcomeApproved && command.Response == contract.ResponseApprove) ||
			(*receipt.Outcome == contract.OutcomeRejected && command.Response == contract.ResponseReject))
	if !shouldApplyDomainAction {
		if err := tx.Commit(); err != nil {
			return contract.ResponseReceipt{}, fmt.Errorf("commit approval response: %w", err)
		}
		if release != nil {
			release()
			release = nil
		}
		return receipt, nil
	}
	_, afterCommit, commandErr := c.engine.ExecuteInTx(ctx, database, tx, c.registry, operation)
	if commandErr != nil {
		return contract.ResponseReceipt{}, commandErr
	}
	if err := tx.Commit(); err != nil {
		return contract.ResponseReceipt{}, fmt.Errorf("commit approval and domain operation: %w", err)
	}
	if release != nil {
		release()
		release = nil
	}
	if afterCommit != nil {
		afterCommit()
	}
	return receipt, nil
}

func workflowOperation(exec policy.Execution, request requests.ResponseContext, workflow WorkflowApproval, requestKey contract.IdempotencyKey) (kernel.Operation, error) {
	if !exec.Valid() || request.Subject == nil || request.PolicyID == "" || workflow.Action == "" {
		return kernel.Operation{}, contract.NewError(contract.CodeValidationFailed, "verified approval subject and registered workflow action are required")
	}
	if _, err := contract.ParseRevision(string(request.Subject.Revision)); err != nil {
		return kernel.Operation{}, contract.NewError(contract.CodeConflict, "approval subject revision is invalid")
	}
	revision, err := strconv.ParseUint(string(request.Subject.Revision), 10, 64)
	if err != nil || revision == 0 {
		return kernel.Operation{}, contract.NewError(contract.CodeConflict, "approval subject revision cannot be used for a domain transition")
	}
	actor := exec.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	primaryRole := ""
	if len(actor.Roles) > 0 {
		primaryRole = actor.Roles[0]
	}
	keyDigest := sha256.Sum256([]byte(string(exec.Tenant()) + "\x00" + actor.PrincipalID + "\x00" + string(request.RequestID) + "\x00" + string(requestKey)))
	key, err := contract.ParseIdempotencyKey("approval:" + hex.EncodeToString(keyDigest[:]))
	if err != nil {
		return kernel.Operation{}, contract.NewError(contract.CodeInternal, "could not derive domain idempotency key")
	}
	payload, err := json.Marshal(kernel.WorkflowStatusTransitionPayload{Doctype: request.Subject.Kind, Name: request.Subject.ID, Action: workflow.Action, ExpectedRevision: revision})
	if err != nil {
		return kernel.Operation{}, contract.NewError(contract.CodeInternal, "could not encode registered workflow transition")
	}
	return kernel.Operation{Command: kernel.CommandWorkflowStatusTransition, Payload: payload, Context: kernel.OperationContext{
		Site: actor.Site, TenantID: exec.Tenant(), Execution: exec, Actor: actor, User: user,
		UserRole: primaryRole, Roles: append([]string(nil), actor.Roles...), Source: kernel.SourceHTTP,
		IdempotencyKey: string(key),
	}}, nil
}
