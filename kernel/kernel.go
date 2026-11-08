// Package kernel implements the single operation kernel (architecture
// specification invariants 2–3, KERNEL-001..007): every mutation from every
// source — HTTP, SDK, MCP, AI, CLI, UI, integrations, workers — enters one
// pipeline that resolves tenant and actor, authorizes, validates, checks
// idempotency and concurrency, executes one SQL transaction containing the
// business mutation plus the idempotency receipt, the audit row, and the
// outbox events, and returns a typed OperationResult.
//
// The kernel never talks to a provider directly; durable delivery happens
// exclusively through the transactional outbox and its publishers.
package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/workflow"
)

// Source identifies the adapter surface that produced an operation. It is
// recorded on the audit ledger and drives authorization-parity tests: two
// sources executing the same command must produce identical outcomes.
type Source string

const (
	SourceHTTP        Source = "http"
	SourceSDK         Source = "sdk"
	SourceMCP         Source = "mcp"
	SourceAI          Source = "ai"
	SourceCLI         Source = "cli"
	SourceUI          Source = "ui"
	SourceWorker      Source = "worker"
	SourceIntegration Source = "integration"
)

// Command names are canonical across all sources. Adapters resolve these
// definitions from the registry; they never implement their own semantics.
const (
	CommandRecordCreate             = "record.create"
	CommandRecordMutateBundle       = "record.mutate_bundle"
	CommandRecordUpdate             = "record.update"
	CommandRecordDelete             = "record.delete"
	CommandWorkflowStatusTransition = "workflow.status_transition"
	CommandPublicFormSubmit         = "public_form.submit"
)

// RecordCreatePayload is the typed payload for record.create. Field values
// are schema-generic by design (the engine is config-driven) but are always
// validated against the doctype definition before execution; invalid shapes
// produce typed ValidationError lists, never silent acceptance.
type RecordCreatePayload struct {
	Doctype     string          `json:"doctype"`
	Data        json.RawMessage `json:"data"`
	PublicRoute string          `json:"public_route,omitempty"`
}

// RecordMutationBundlePayload creates and updates a small set of related
// records as one business operation. Records may refer to each other's names
// with values such as "$records.sale.name". The kernel owns validation, hooks,
// audit, outbox, receipts, and the single transaction for the whole bundle.

type RecordMutationBundlePayload struct {
	Doctype string                     `json:"doctype"`
	Records []RecordMutationBundleItem `json:"records"`
}

// MaxMutationBundleRecords bounds the number of related writes in one
// transactional command while leaving room for ordinary multi-line checkouts.
const MaxMutationBundleRecords = 50

type RecordMutationBundleItem struct {
	Key              string          `json:"key"`
	Operation        string          `json:"operation,omitempty"` // create (default), update, decrement, or adjust
	Doctype          string          `json:"doctype"`
	Name             string          `json:"name,omitempty"`
	ExpectedRevision uint64          `json:"expected_revision,omitempty"`
	TransitionAction string          `json:"transition_action,omitempty"` // configured transition, only valid on updates
	Data             json.RawMessage `json:"data"`
}

type RelatedRecordResult struct {
	Key     string `json:"key"`
	Doctype string `json:"doctype"`
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

// RecordUpdatePayload is the typed payload for record.update.
type RecordUpdatePayload struct {
	Doctype string          `json:"doctype"`
	Name    string          `json:"name"`
	Data    json.RawMessage `json:"data"`
}

// WorkflowStatusTransitionPayload applies a configured transition to one
// record without exposing its workflow state as an ordinary field update.
type WorkflowStatusTransitionPayload struct {
	Doctype          string `json:"doctype"`
	Name             string `json:"name"`
	Action           string `json:"action"`
	ExpectedRevision uint64 `json:"expected_revision,omitempty"`
}

// CommandDefinition is the single registered description of a command:
// its payload shape, authorization operation, idempotency class, and timeout.
type CommandDefinition struct {
	Name            string
	Version         int
	AuthorizesWith  string // permission-matrix operation ("create", "write")
	IdempotentByKey bool   // replay-safe when an idempotency key is present
}

// commandRegistry holds the canonical command definitions of the first
// vertical slice. Additional commands register here as epics land.
var commandRegistry = map[string]CommandDefinition{
	CommandRecordCreate:             {Name: CommandRecordCreate, Version: 1, AuthorizesWith: "create", IdempotentByKey: true},
	CommandRecordMutateBundle:       {Name: CommandRecordMutateBundle, Version: 1, AuthorizesWith: "create", IdempotentByKey: true},
	CommandRecordUpdate:             {Name: CommandRecordUpdate, Version: 1, AuthorizesWith: "write", IdempotentByKey: true},
	CommandRecordDelete:             {Name: CommandRecordDelete, Version: 1, AuthorizesWith: "delete", IdempotentByKey: true},
	CommandWorkflowStatusTransition: {Name: CommandWorkflowStatusTransition, Version: 1, AuthorizesWith: "submit", IdempotentByKey: true},
	CommandPublicFormSubmit:         {Name: CommandPublicFormSubmit, Version: 1, AuthorizesWith: "create", IdempotentByKey: true},
}

// LookupCommand returns the canonical definition for a command name.
func LookupCommand(name string) (CommandDefinition, bool) {
	d, ok := commandRegistry[name]
	return d, ok
}

// OperationContext carries everything an adapter must resolve before invoking
// the kernel. It is an explicit value, never smuggled through context.Context.
type OperationContext struct {
	ModuleExecution             *contract.ModuleExecution
	BusinessScope               contract.BusinessScope
	BusinessReferenceValidator  contract.BusinessReferenceValidator
	HistoricalSnapshotValidator contract.HistoricalSnapshotValidator
	Site                        string                // authenticated tenant route, retained for audit and compatibility
	TenantID                    contract.TenantID     // stable tenant identity from routed runtime
	Execution                   policy.Execution      // verified identity; legacy internal callers are being migrated
	Actor                       contract.ActorContext // authenticated principal
	User                        string                // owner-attribution user ("" → "system")
	Owner                       string                // internal owner override; adapters must not populate this from user data
	UserRole                    string                // primary role used by workflow policy
	Roles                       []string              // roles used for permission-matrix lookup
	SkipHookScripts             []string              // trusted internal stack prevents recursive execution of active lifecycle scripts
	AllowReadOnlyFields         bool                  // trusted internal writers may update UI-read-only fields
	Source                      Source                // adapter surface
	CorrelationID               string
	CausationID                 string
	CausationDepth              uint16
	ExpectedVersion             string
	// ExpectedConfigRevision fences internal operations that selected behavior
	// from tenant configuration before entering the source transaction.
	ExpectedConfigRevision uint64
	// ConfigRevision pins immutable behavior for durable asynchronous effects;
	// it is distinct from the caller's expected-revision precondition.
	ConfigRevision uint64
	IdempotencyKey string
	// Cross-entity operation state is derived from a registered command and
	// verified source scope inside Kernel.Execute; adapters cannot populate it.
	crossEntityOperation *contract.CrossEntityOperation
	crossEntityTarget    contract.BusinessScope
	// effectItems names the bundle items the kernel itself added to apply a
	// record's quantity effect. They are authorized by the permission to create
	// the source record, not by write permission on the target. Only
	// prepareQuantityEffect sets this; adapters and payloads cannot.
	effectItems map[string]struct{}
}

func (c OperationContext) isEffectItem(key string) bool {
	_, ok := c.effectItems[key]
	return ok
}

// Operation is a fully specified invocation handed to Kernel.Execute.
type Operation struct {
	Context  OperationContext
	Command  string          // canonical command name
	Payload  json.RawMessage // JSON-encoded typed payload
	Deadline time.Time       // zero means no deadline; enforced before execution
}

// ResultData is the typed success payload returned by record commands.
type ResultData struct {
	Doctype   string                `json:"doctype"`
	Name      string                `json:"name"`
	Created   bool                  `json:"created"`
	Deleted   bool                  `json:"deleted,omitempty"`
	Document  map[string]any        `json:"document,omitempty"`
	AuditID   string                `json:"audit_id"`
	Operation string                `json:"operation_id"`
	Related   []RelatedRecordResult `json:"related,omitempty"`
}

// Kernel is the process-wide operation entry point. It is safe for concurrent
// use; per-site state (DB handle, registry) is supplied per call via SiteDB.
type Kernel struct {
	Dialect                db.Dialect
	Outbox                 outbox.Writer
	WorkflowStatusPolicies workflow.StatusPolicyRepository

	// TxManager supplies request-scoped ORM lifecycle hooks and script runtime.
	// Database, registry, dialect, site, user, and outbox are rebound per Execute.
	TxManager *orm.TxManager

	// Commands holds config-defined command resources (KERNEL-008). Nil means
	// only built-in commands are available. Definitions are generation-scoped:
	// swap the registry atomically on activation.
	Commands *CommandRegistry

	// CommandSchemas is the existing versioned schema registry used by
	// module-owned commands. The same validator is applied for provider and
	// direct command entry paths before transactional effects can commit.
	CommandSchemas CommandSchemaRegistry
}

// New returns a Kernel. Outbox may be nil only in tests that do not assert
// event delivery; production wiring always supplies outbox.NewSQLWriter().
func New(dialect db.Dialect, w outbox.Writer) *Kernel {
	return &Kernel{Dialect: dialect, Outbox: w}
}

// Execute runs the full pipeline for op against the given site database and
// registry. Exactly one of the following occurs:
//
//   - completed: business row + receipt + audit + outbox rows committed atomically;
//   - replayed:  a prior committed result for the same stable tenant+idempotency key is returned;
//   - rejected:  a typed error; nothing is persisted (transaction rolled back).
func (k *Kernel) Execute(ctx context.Context, siteDB *sql.DB, reg *doctype.Registry, op Operation) (contract.CommandResult, *contract.Error) {
	opID := contract.NewOperationID()
	if !op.Context.Execution.Valid() {
		return k.rejected(opID, op, contract.NewError(contract.CodeUnauthenticated, "operation requires verified execution identity"))
	}
	if err := normalizeVerifiedExecution(&op); err != nil {
		return k.rejected(opID, op, err)
	}

	if err := k.validateContext(op); err != nil {
		return k.rejected(opID, op, err)
	}
	if !op.Deadline.IsZero() && !time.Now().Before(op.Deadline) {
		return k.rejected(opID, op, contract.NewError(contract.CodeDeadlineExceeded, "operation deadline exceeded"))
	}
	if err := prepareQuantityEffect(reg, &op); err != nil {
		return k.rejected(opID, op, err)
	}
	def, ok := LookupCommand(op.Command)
	var dyn *CommandResource
	if !ok {
		if c, found := k.Commands.Lookup(op.Command); found {
			dyn = c
			def = CommandDefinition{
				Name:            c.FullName(),
				Version:         c.Version,
				AuthorizesWith:  c.PermOperation(),
				IdempotentByKey: true,
			}
		} else {
			return k.rejected(opID, op, contract.NewError(contract.CodeValidationFailed, "unknown command "+op.Command))
		}
	}
	if err := prepareCrossEntityCommand(&op, dyn); err != nil {
		return k.rejected(opID, op, err)
	}

	// Authorization parity gate: identical evaluation regardless of source.
	authzErr := authorizeOp(reg, op, def, dyn)
	if authzErr != nil {
		k.writeDenialAudit(siteDB, op, def, opID, authzErr)
		return k.rejected(opID, op, authzErr)
	}

	// Idempotency fast path: return a committed result without re-executing.
	if op.Context.IdempotencyKey != "" {
		res, found, err := k.lookupReceipt(ctx, siteDB, op)
		if err != nil {
			return k.rejected(opID, op, err)
		}
		if found {
			return res, nil
		}
	}

	txMgr := &orm.TxManager{
		DB:               siteDB,
		Registry:         reg,
		Dialect:          k.Dialect,
		Outbox:           k.Outbox,
		SiteName:         op.Context.Site,
		CurrentUser:      op.Context.User,
		Execution:        op.Context.Execution,
		BusinessScope:    op.Context.BusinessScope,
		CurrentUserRole:  op.Context.UserRole,
		CurrentUserRoles: append([]string(nil), op.Context.Roles...),
	}
	if k.TxManager != nil {
		*txMgr = *k.TxManager
		txMgr.DB = siteDB
		txMgr.Registry = reg
		txMgr.Dialect = k.Dialect
		txMgr.Outbox = k.Outbox
		txMgr.SiteName = op.Context.Site
		txMgr.CurrentUser = op.Context.User
		txMgr.BusinessScope = op.Context.BusinessScope
	}
	if txMgr.WorkflowStatusPolicies == nil {
		txMgr.WorkflowStatusPolicies = k.WorkflowStatusPolicies
	}
	baseAsyncConfigRevision := txMgr.AsyncHookConfigRevision
	txMgr.Execution = op.Context.Execution
	txMgr.CurrentUserRole = op.Context.UserRole
	txMgr.CurrentUserRoles = append([]string(nil), op.Context.Roles...)
	txMgr.SkipHookScripts = append([]string(nil), op.Context.SkipHookScripts...)
	txMgr.HookScriptsByDoctype = make(map[string][]script.ScriptRecord)
	txMgr.AsyncHookTenantID = op.Context.TenantID
	txMgr.AsyncHookSourceOperation = contract.OperationID(opID)
	txMgr.AsyncHookConfigRevision = op.Context.ConfigRevision
	if txMgr.AsyncHookConfigRevision == 0 {
		txMgr.AsyncHookConfigRevision = op.Context.ExpectedConfigRevision
	}
	if txMgr.AsyncHookConfigRevision == 0 {
		txMgr.AsyncHookConfigRevision = baseAsyncConfigRevision
	}
	txMgr.AsyncHookSequence = 0
	txMgr.SuppressAsyncHookEvents = false
	bindScriptProvider(txMgr)

	if dyn != nil {
		data, execErr := k.execDefinedCommand(ctx, siteDB, nil, txMgr, reg, op, def, opID, dyn, nil)
		if execErr != nil {
			if execErr == errConcurrentIdempotencyClaim {
				if replay, found, lookupErr := k.lookupReceipt(ctx, siteDB, op); lookupErr != nil {
					return k.rejected(opID, op, lookupErr)
				} else if found {
					return replay, nil
				}
			}
			return k.rejected(opID, op, execErr)
		}
		result := contract.CommandResult{
			OperationID:   opID,
			CorrelationID: op.Context.CorrelationID,
			Status:        contract.StatusCompleted,
			Data:          data,
		}
		return result, nil
	}

	data, execErr := k.executeInTx(ctx, siteDB, nil, txMgr, reg, op, def, opID, nil)
	if execErr != nil {
		if execErr == errConcurrentIdempotencyClaim {
			if replay, found, lookupErr := k.lookupReceipt(ctx, siteDB, op); lookupErr != nil {
				return k.rejected(opID, op, lookupErr)
			} else if found {
				return replay, nil
			}
		}
		return k.rejected(opID, op, execErr)
	}

	result := contract.CommandResult{
		OperationID:   opID,
		CorrelationID: op.Context.CorrelationID,
		Status:        contract.StatusCompleted,
	}
	if data != nil {
		raw, _ := json.Marshal(data)
		result.Data = raw
	}

	return result, nil
}

// ExecuteInTx prepares and executes an authorized command inside a transaction
// owned by a higher-level unit of work. It does not commit or roll back dbTx.
// The caller must roll back on error, commit on success, and invoke afterCommit
// only after that commit succeeds. The verified execution is mandatory so a
// caller-supplied actor cannot acquire authority through this internal seam.
func (k *Kernel) ExecuteInTx(ctx context.Context, siteDB *sql.DB, dbTx *sql.Tx, reg *doctype.Registry, op Operation) (contract.CommandResult, func(), *contract.Error) {
	if ctx == nil || siteDB == nil || dbTx == nil {
		return contract.CommandResult{}, nil, contract.NewError(contract.CodeValidationFailed, "transaction-bound command requires context, database and transaction")
	}
	if !op.Context.Execution.Valid() {
		return contract.CommandResult{}, nil, contract.NewError(contract.CodeUnauthenticated, "transaction-bound command requires verified execution identity")
	}
	opID := contract.NewOperationID()
	if err := normalizeVerifiedExecution(&op); err != nil {
		return contract.CommandResult{}, nil, err
	}
	if err := k.validateContext(op); err != nil {
		return contract.CommandResult{}, nil, err
	}
	if !op.Deadline.IsZero() && !time.Now().Before(op.Deadline) {
		return contract.CommandResult{}, nil, contract.NewError(contract.CodeDeadlineExceeded, "operation deadline exceeded")
	}
	if err := prepareQuantityEffect(reg, &op); err != nil {
		return contract.CommandResult{}, nil, err
	}
	def, ok := LookupCommand(op.Command)
	var dyn *CommandResource
	if !ok {
		if command, found := k.Commands.Lookup(op.Command); found {
			dyn = command
			def = CommandDefinition{Name: command.FullName(), Version: command.Version, AuthorizesWith: command.PermOperation(), IdempotentByKey: true}
		} else {
			return contract.CommandResult{}, nil, contract.NewError(contract.CodeValidationFailed, "unknown command "+op.Command)
		}
	}
	if err := prepareCrossEntityCommand(&op, dyn); err != nil {
		return contract.CommandResult{}, nil, err
	}
	if authzErr := authorizeOp(reg, op, def, dyn); authzErr != nil {
		return contract.CommandResult{}, nil, authzErr
	}
	if op.Context.IdempotencyKey != "" {
		result, found, lookupErr := k.lookupReceiptTx(ctx, dbTx, op)
		if lookupErr != nil {
			return contract.CommandResult{}, nil, lookupErr
		}
		if found {
			return result, func() {}, nil
		}
	}

	txMgr := &orm.TxManager{DB: siteDB, Registry: reg, Dialect: k.Dialect, Outbox: k.Outbox,
		SiteName: op.Context.Site, CurrentUser: op.Context.User, Context: ctx,
		CurrentUserRole: op.Context.UserRole, CurrentUserRoles: append([]string(nil), op.Context.Roles...), Execution: op.Context.Execution, BusinessScope: op.Context.BusinessScope}
	if k.TxManager != nil {
		*txMgr = *k.TxManager
		txMgr.DB, txMgr.Registry, txMgr.Dialect, txMgr.Outbox = siteDB, reg, k.Dialect, k.Outbox
		txMgr.SiteName, txMgr.CurrentUser, txMgr.Context = op.Context.Site, op.Context.User, ctx
		txMgr.CurrentUserRole = op.Context.UserRole
		txMgr.CurrentUserRoles = append([]string(nil), op.Context.Roles...)
		txMgr.Execution = op.Context.Execution
		txMgr.BusinessScope = op.Context.BusinessScope
	}
	baseAsyncConfigRevision := txMgr.AsyncHookConfigRevision
	txMgr.SkipHookScripts = append([]string(nil), op.Context.SkipHookScripts...)
	txMgr.HookScriptsByDoctype = make(map[string][]script.ScriptRecord)
	txMgr.AsyncHookTenantID = op.Context.TenantID
	txMgr.AsyncHookSourceOperation = contract.OperationID(opID)
	txMgr.AsyncHookConfigRevision = op.Context.ConfigRevision
	if txMgr.AsyncHookConfigRevision == 0 {
		txMgr.AsyncHookConfigRevision = op.Context.ExpectedConfigRevision
	}
	if txMgr.AsyncHookConfigRevision == 0 {
		txMgr.AsyncHookConfigRevision = baseAsyncConfigRevision
	}
	txMgr.AsyncHookSequence = 0
	txMgr.SuppressAsyncHookEvents = false
	bindScriptProvider(txMgr)
	var afterCommit func()
	if dyn != nil {
		data, execErr := k.execDefinedCommand(ctx, siteDB, dbTx, txMgr, reg, op, def, opID, dyn, &afterCommit)
		if execErr != nil {
			return contract.CommandResult{}, nil, execErr
		}
		return contract.CommandResult{OperationID: opID, CorrelationID: op.Context.CorrelationID, Status: contract.StatusCompleted, Data: data}, afterCommit, nil
	}
	data, execErr := k.executeInTx(ctx, siteDB, dbTx, txMgr, reg, op, def, opID, &afterCommit)
	if execErr != nil {
		return contract.CommandResult{}, nil, execErr
	}
	result := contract.CommandResult{OperationID: opID, CorrelationID: op.Context.CorrelationID, Status: contract.StatusCompleted}
	if data != nil {
		result.Data, _ = json.Marshal(data)
	}
	return result, afterCommit, nil
}

func bindScriptProvider(txMgr *orm.TxManager) {
	if txMgr != nil && txMgr.ScriptProviderFactory != nil {
		txMgr.ScriptProvider = txMgr.ScriptProviderFactory(txMgr)
	}
}

func normalizeVerifiedExecution(op *Operation) *contract.Error {
	if op == nil || !op.Context.Execution.Valid() {
		return nil
	}
	execution := op.Context.Execution
	actor := execution.Actor()
	if op.Context.TenantID != "" && op.Context.TenantID != execution.Tenant() {
		return contract.NewError(contract.CodePermissionDenied, "operation tenant ID does not match verified execution")
	}
	if op.Context.Site != "" && op.Context.Site != actor.Site {
		return contract.NewError(contract.CodePermissionDenied, "operation tenant route does not match verified actor")
	}
	op.Context.TenantID, op.Context.Site, op.Context.Actor = execution.Tenant(), actor.Site, actor
	if op.Context.BusinessScope.Kind == "" && op.Context.BusinessScope.TenantID == "" && op.Context.BusinessScope.LegalEntityID == "" {
		op.Context.BusinessScope = contract.BusinessScope{TenantID: execution.Tenant(), Kind: contract.ScopeKindTenant}
	} else if err := op.Context.BusinessScope.Validate(); err != nil || op.Context.BusinessScope.TenantID != execution.Tenant() {
		return contract.NewError(contract.CodePermissionDenied, "operation business scope does not match verified tenant")
	}
	op.Context.Roles = append([]string(nil), actor.Roles...)
	op.Context.UserRole = ""
	if len(actor.Roles) > 0 {
		op.Context.UserRole = actor.Roles[0]
	}
	op.Context.User = actor.SubjectUserID
	if op.Context.User == "" {
		op.Context.User = actor.PrincipalID
	}
	return nil
}

// dialect returns the configured SQL dialect for placeholder rewriting.
// A nil dialect falls back to MySQL-style '?' placeholders.
func (k *Kernel) dialect() db.Dialect {
	if k.Dialect == nil {
		return &db.MySQLDialect{}
	}
	return k.Dialect
}
