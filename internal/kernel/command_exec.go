package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

// admissionScope is what module-lifecycle admission looks up: the schema
// registry, the SQL dialect, and the config-defined command, if any.
type admissionScope struct {
	Dialect  db.Dialect
	Registry *doctype.Registry
	Dynamic  *CommandResource
}

// enforceModuleLifecycleAdmissionTx serializes module-owned command admission
// with lifecycle transitions by touching the snapshot row inside the business
// transaction. Historical reads do not pass through this write guard.
func enforceModuleLifecycleAdmissionTx(ctx context.Context, tx *sql.Tx, op Operation, scope admissionScope) *contract.Error {
	dialect, reg, dynamic := scope.Dialect, scope.Registry, scope.Dynamic
	if tx == nil || reg == nil {
		return nil
	}
	owners := map[contract.ModuleID]struct{}{}
	for _, name := range operationTouchedRecords(op, dynamic) {
		if dt := reg.Get(name); dt != nil && dt.ModuleOwnerID != "" {
			owners[contract.ModuleID(dt.ModuleOwnerID)] = struct{}{}
		}
	}
	if len(owners) == 0 {
		return nil
	}
	if dialect == nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "module lifecycle state is unavailable")
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_module_snapshot SET revision = revision WHERE tenant_id = ?`), string(op.Context.TenantID)); err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "module lifecycle admission could not be fenced")
	}
	var payload string
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`), string(op.Context.TenantID)).Scan(&payload); err != nil {
		return contract.NewError(contract.CodePermissionDenied, "owning module is not installed or available")
	}
	var snapshot contract.ModuleSnapshot
	if json.Unmarshal([]byte(payload), &snapshot) != nil || snapshot.TenantID != op.Context.TenantID {
		return contract.NewError(contract.CodeDependencyUnavailable, "module lifecycle snapshot is invalid")
	}
	for owner := range owners {
		found := false
		for _, installation := range snapshot.Installations {
			if installation.ModuleID != owner {
				continue
			}
			found = true
			if installation.State != string(contract.ModuleActive) && (installation.State != string(contract.ModuleDraining) || !pinnedWorkflowAttemptPredatesDrain(ctx, tx, dialect, op, installation.StateChangedAt)) {
				return contract.NewError(contract.CodePermissionDenied, "owning module is not accepting new commands")
			}
			break
		}
		if !found {
			return contract.NewError(contract.CodePermissionDenied, "owning module is not installed")
		}
	}
	return nil
}

func pinnedWorkflowAttemptPredatesDrain(ctx context.Context, tx *sql.Tx, dialect db.Dialect, op Operation, drainingAt time.Time) bool {
	if drainingAt.IsZero() || op.Context.ModuleExecution == nil {
		return false
	}
	pin, ok := op.Context.ModuleExecution.WorkflowCommandPin()
	if !ok || pin.IdempotencyKey == "" || pin.InstanceID == "" || pin.StepID == "" {
		return false
	}
	var started any
	err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT MIN(started_at) FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? AND idempotency_key = ?`), string(op.Context.TenantID), string(pin.InstanceID), string(pin.StepID), string(pin.IdempotencyKey)).Scan(&started)
	if err != nil || started == nil {
		return false
	}
	var startedAt time.Time
	switch value := started.(type) {
	case time.Time:
		startedAt = value
	case string:
		startedAt, err = time.Parse(time.RFC3339Nano, value)
	case []byte:
		startedAt, err = time.Parse(time.RFC3339Nano, string(value))
	default:
		return false
	}
	return err == nil && !startedAt.IsZero() && !startedAt.After(drainingAt)
}

func operationTouchedRecords(op Operation, dynamic *CommandResource) []string {
	if dynamic != nil {
		return dynamic.TouchedRecords()
	}
	if op.Command == CommandRecordMutateBundle {
		var payload RecordMutationBundlePayload
		if json.Unmarshal(op.Payload, &payload) == nil {
			names := make([]string, 0, len(payload.Records))
			for _, record := range payload.Records {
				names = append(names, record.Doctype)
			}
			return names
		}
	}
	if name := payloadDoctype(op.Payload); name != "" {
		return []string{name}
	}
	return nil
}

// authorizeOp dispatches authorization: built-in commands authorize against
// the payload's doctype; config-defined commands authorize EVERY touched
// record at per-step least privilege — create steps need "create", update
// steps need "write" on their target record (default-deny, spec §51/§108).
func authorizeOp(reg *doctype.Registry, op Operation, def CommandDefinition, dyn *CommandResource) *contract.Error {
	if dyn != nil && dyn.CrossEntity != nil {
		if op.Context.crossEntityOperation == nil {
			return contract.NewError(contract.CodePermissionDenied, "cross-entity command requires verified source and target scopes")
		}
		if authErr := policy.AuthorizeCrossEntityOperation(op.Context.Execution, *op.Context.crossEntityOperation, reg); authErr != nil {
			return authErr
		}
	}
	if dyn != nil && dynamicCommandTouchesCommandOnly(reg, dyn) {
		if !validDynamicModuleExecution(op.Context.ModuleExecution, op, reg, dyn) {
			return contract.NewError(contract.CodePermissionDenied, "protected resources require their owning module command")
		}
	}
	if mutationTouchesCommandOnly(reg, op) {
		execution := op.Context.ModuleExecution
		if execution == nil || execution.AuthorizedCommand == "" || execution.AuthorizedCommand == CommandRecordCreate || execution.AuthorizedCommand == CommandRecordUpdate || execution.AuthorizedCommand == CommandRecordMutateBundle || execution.AuthorizedCommand == CommandRecordDelete {
			return contract.NewError(contract.CodePermissionDenied, "resource is owned by a protected module and requires its domain command")
		}
		if !validModuleExecution(execution, op, reg) {
			return contract.NewError(contract.CodePermissionDenied, "module execution is not authorized for this domain command")
		}
	}
	if op.Command == CommandPublicFormSubmit {
		return authorizePublicForm(reg, op)
	}
	if op.Command == CommandRecordMutateBundle {
		if reg == nil {
			return ErrPermission
		}
		var payload RecordMutationBundlePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.Doctype == "" || len(payload.Records) == 0 || len(payload.Records) > MaxMutationBundleRecords {
			return contract.NewError(contract.CodeValidationFailed, "record.mutate_bundle requires 1 to 50 records")
		}
		for _, record := range payload.Records {
			if record.Doctype == "" {
				return contract.NewError(contract.CodeValidationFailed, "bundle record doctype is required")
			}
			dt := reg.Get(record.Doctype)
			if dt == nil {
				return contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", record.Doctype))
			}
			if dt.IsChildTable {
				return contract.NewError(contract.CodeValidationFailed, "child doctypes cannot be standalone bundle records")
			}
			permission := record.Operation
			if permission == "" {
				permission = "create"
			}
			if permission != "create" && permission != "update" && permission != "decrement" && permission != "adjust" {
				return contract.NewError(contract.CodeValidationFailed, "bundle operation must be create, update, decrement, or adjust")
			}
			if permission != "create" && record.Name == "" {
				return contract.NewError(contract.CodeValidationFailed, "bundle update requires a record name")
			}
			if dt.AppendOnly && permission != "create" {
				return contract.NewError(contract.CodeValidationFailed, "append-only records cannot be changed")
			}
			if permission != "create" {
				permission = "write"
			}
			if !op.Context.isEffectItem(record.Key) {
				if _, err := decideOperationResource(op, reg, dt, permission); err != nil {
					return err
				}
			}
		}
		root := reg.Get(payload.Doctype)
		if root == nil || root.Name != reg.Get(payload.Records[0].Doctype).Name {
			return contract.NewError(contract.CodeValidationFailed, "bundle root doctype must match the first record")
		}
		return nil
	}
	if dyn == nil {
		return authorize(reg, op, def)
	}
	if reg == nil {
		return ErrPermission
	}
	// A published operation called through its verified module execution runs
	// with the operation's authority: the caller's operation grant is checked
	// once, then steps on records the command's owner module owns (any mode)
	// or that no module owns skip the caller's matrix. Records owned by
	// another module keep their protection. Without a verified execution
	// (direct kernel route, scripts) every step uses the actor's matrix.
	moduleCommandAuthorized := validPublishedExecution(op.Context.ModuleExecution, op, reg, dyn)
	if moduleCommandAuthorized {
		resource := "module." + string(dyn.ModuleOwnerID) + ".operation." + dyn.Operation
		if _, authErr := policy.DecideForBusinessScope(op.Context.Execution, reg, resource, "write", op.Context.BusinessScope, op.Context.BusinessScope.Kind == contract.ScopeKindLegalEntity); authErr != nil {
			return authErr
		}
	}
	operationAuthority := func(dt *doctype.DocType) bool {
		return moduleCommandAuthorized && (dt.ModuleOwnerID == "" || contract.ModuleID(dt.ModuleOwnerID) == dyn.ModuleOwnerID)
	}
	if authErr := dyn.authorizeStepFieldReads(reg, op, operationAuthority); authErr != nil {
		return authErr
	}
	for _, step := range dyn.Steps {
		if alias, ok := iteratedGuardAlias(step); ok {
			dt := dyn.guardRecord(reg, alias)
			if dt == nil {
				return contract.NewError(contract.CodeNotFound, "for_each guard record not found")
			}
			if !operationAuthority(dt) {
				if _, err := decideOperationResource(op, reg, dt, "read"); err != nil {
					return err
				}
			}
		}
		stepOp, scopeErr := operationForCommandStep(op, dyn, step)
		if scopeErr != nil {
			return scopeErr
		}
		switch {
		case step.createSpec() != nil:
			create := step.createSpec()
			dt := reg.Get(create.Record)
			if dt == nil {
				return contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", create.Record))
			}
			if operationAuthority(dt) {
				continue
			}
			if _, err := decideOperationResource(stepOp, reg, dt, "create"); err != nil {
				return err
			}
		case step.Update != nil:
			dt := reg.Get(step.Update.Record)
			if dt == nil {
				return contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", step.Update.Record))
			}
			if dt.AppendOnly {
				return contract.NewError(contract.CodeValidationFailed, "append-only records cannot be changed")
			}
			if operationAuthority(dt) {
				continue
			}
			if _, err := decideOperationResource(stepOp, reg, dt, "write"); err != nil {
				return err
			}
		}
	}
	return nil
}

func dynamicCommandTouchesCommandOnly(reg *doctype.Registry, command *CommandResource) bool {
	if reg == nil || command == nil {
		return false
	}
	for _, name := range command.TouchedRecords() {
		dt := reg.Get(name)
		if dt != nil && dt.ModuleOwnershipMode == string(contract.MutationCommands) {
			return true
		}
	}
	return false
}

// validPublishedExecution reports whether execution is a verified, sealed
// module execution of this published command for its own owner module.
func validPublishedExecution(execution *contract.ModuleExecution, op Operation, reg *doctype.Registry, command *CommandResource) bool {
	return validDynamicBase(execution, op, reg, command) && execution.OwnerModuleID == command.ModuleOwnerID
}

func validDynamicModuleExecution(execution *contract.ModuleExecution, op Operation, reg *doctype.Registry, command *CommandResource) bool {
	if !validDynamicBase(execution, op, reg, command) {
		return false
	}
	protected := false
	for _, name := range command.TouchedRecords() {
		dt := reg.Get(name)
		if dt == nil {
			return false
		}
		if dt.ModuleOwnershipMode == string(contract.MutationCommands) {
			protected = true
			if dt.ModuleOwnerID == "" || execution.OwnerModuleID != contract.ModuleID(dt.ModuleOwnerID) || command.ModuleOwnerID != execution.OwnerModuleID {
				return false
			}
		}
	}
	return protected
}

func validDynamicBase(execution *contract.ModuleExecution, op Operation, reg *doctype.Registry, command *CommandResource) bool {
	return execution != nil && execution.Verified() && execution.TenantID == op.Context.TenantID && execution.Scope == op.Context.BusinessScope && execution.AuthorizedCommand == op.Command && execution.AllowedOperation == op.Command && command != nil && command.FullName() == op.Command && command.ModuleOwnerID != "" && command.Capability != "" && command.Operation != "" && command.InputSchema.ID != "" && command.InputSchema.Version != 0 && command.OutputSchema.ID != "" && command.OutputSchema.Version != 0 && reg != nil
}

func validModuleExecution(execution *contract.ModuleExecution, op Operation, reg *doctype.Registry) bool {
	if execution == nil || !execution.Verified() || execution.TenantID != op.Context.TenantID || execution.Scope != op.Context.BusinessScope {
		return false
	}
	if execution.AuthorizedCommand == "" || execution.AuthorizedCommand != op.Command || execution.AllowedOperation != op.Command {
		return false
	}
	dt := reg.Get(payloadDoctype(op.Payload))
	if dt == nil {
		return false
	}
	return execution.OwnerModuleID == contract.ModuleID(dt.ModuleOwnerID)
}

func mutationTouchesCommandOnly(reg *doctype.Registry, op Operation) bool {
	if reg == nil {
		return false
	}
	check := func(name string) bool {
		dt := reg.Get(name)
		if dt == nil {
			return false
		}
		return dt.ModuleOwnershipMode == string(contract.MutationCommands)
	}
	switch op.Command {
	case CommandRecordCreate, CommandRecordUpdate, CommandRecordDelete, CommandWorkflowStatusTransition, CommandPublicFormSubmit:
		return check(payloadDoctype(op.Payload))
	case CommandRecordMutateBundle:
		var payload RecordMutationBundlePayload
		if json.Unmarshal(op.Payload, &payload) != nil {
			return false
		}
		for _, record := range payload.Records {
			if check(record.Doctype) {
				return true
			}
		}
	}
	return false
}

func authorizePublicForm(reg *doctype.Registry, op Operation) *contract.Error {
	if reg == nil || op.Context.Actor.PrincipalType != contract.PrincipalPublic {
		return ErrPermission
	}
	var payload RecordCreatePayload
	if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.PublicRoute == "" || payload.Doctype == "" {
		return contract.NewError(contract.CodeValidationFailed, "public form route and doctype are required")
	}
	if reg.Views == nil {
		return ErrPermission
	}
	view := reg.Views.GetByRoute(payload.PublicRoute)
	dt := reg.Get(payload.Doctype)
	if view == nil || view.PublicAccess == nil || !view.PublicAccess.Enabled || !view.PublicAccess.AllowMutations || view.SourceDocType != payload.Doctype || dt == nil || dt.PublicAccess == nil || !dt.PublicAccess.Enabled {
		return ErrPermission
	}
	if dt.BusinessScopePolicy == contract.ScopeEntityOwned {
		return ErrPermission
	}
	return nil
}

type stepOutcome struct {
	Record  string `json:"record"`
	Name    string `json:"name"`
	Created bool   `json:"created"`
}

// execDefinedCommand runs a config-defined command inside ONE SQL transaction:
// receipt claim, per-step writes (each validated against its target record
// schema), emitted events into the outbox, and the audit row — all commit or
// roll back together.
func (k *Kernel) execDefinedCommand(ctx context.Context, run txRun, op Operation, def CommandDefinition, cmd *CommandResource) (json.RawMessage, *contract.Error) { //nolint:contextcheck // the context travels in txMgr.Context
	ownsTransaction := run.Tx == nil
	auditFailure := func(code contract.Code) {
		if ownsTransaction {
			k.writeFailureAudit(run.SiteDB, op, def, run.OpID, code)
		}
	}
	run, rollback, beginErr := run.begin(ctx)
	if beginErr != nil {
		return nil, beginErr
	}
	defer rollback()
	dbTx, txMgr, reg, opID, afterCommit := run.Tx, run.Mgr, run.Reg, run.OpID, run.AfterCommit
	if admissionErr := enforceModuleLifecycleAdmissionTx(ctx, dbTx, op, admissionScope{Dialect: k.dialect(), Registry: reg, Dynamic: cmd}); admissionErr != nil {
		return nil, admissionErr
	}
	priorMutationExecutor := txMgr.ScriptMutationExecutor
	priorPostCommitHooks := txMgr.PostCommitHooks
	priorTransaction := txMgr.Transaction
	priorUser, priorRole, priorRoles, priorContext := txMgr.CurrentUser, txMgr.CurrentUserRole, txMgr.CurrentUserRoles, txMgr.Context
	priorBusinessScope := txMgr.BusinessScope
	priorSkipHooks := txMgr.SkipHookScripts
	priorEventOrigin := txMgr.EventOrigin
	txMgr.PostCommitHooks = nil
	txMgr.Transaction = dbTx
	txMgr.EventOrigin = eventOrigin(op)
	txMgr.CurrentUser = op.Context.User
	txMgr.CurrentUserRole = op.Context.UserRole
	txMgr.CurrentUserRoles = append([]string(nil), op.Context.Roles...)
	txMgr.SkipHookScripts = append([]string(nil), op.Context.SkipHookScripts...)
	txMgr.Context = ctx
	txMgr.ScriptMutationExecutor = script.MutationExecutorFunc(func(nestedCtx context.Context, request script.MutationRequest) (json.RawMessage, error) {
		return k.executeScriptMutationInTx(nestedCtx, dbTx, txMgr, request)
	})
	defer func() {
		txMgr.ScriptMutationExecutor = priorMutationExecutor
		txMgr.PostCommitHooks = priorPostCommitHooks
		txMgr.Transaction = priorTransaction
		txMgr.EventOrigin = priorEventOrigin
		txMgr.CurrentUser, txMgr.CurrentUserRole, txMgr.CurrentUserRoles, txMgr.Context = priorUser, priorRole, priorRoles, priorContext
		txMgr.BusinessScope = priorBusinessScope
		txMgr.SkipHookScripts = priorSkipHooks
	}()

	input, cerr := decodeInput(op.Payload)
	if cerr != nil {
		return nil, cerr
	}
	if cmd.ModuleOwnerID != "" {
		// Owner commands publish their inputs: a missing one is the caller's
		// field-level mistake, reported before the opaque schema failure.
		if cerr := cmd.missingInput(input); cerr != nil {
			return nil, cerr
		}
		if k.CommandSchemas == nil || !k.CommandSchemas.Registered(cmd.InputSchema) || !k.CommandSchemas.Registered(cmd.OutputSchema) {
			return nil, contract.NewError(contract.CodeValidationFailed, "owner command input or output schema is not registered")
		}
		// Module command schemas describe the business input exposed by the
		// published operation. The kernel's {"data":{...}} envelope is an
		// internal command transport detail and must not become part of that
		// public schema contract.
		inputPayload, _ := json.Marshal(input)
		if err := k.CommandSchemas.Validate(cmd.InputSchema, inputPayload); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "owner command input failed its registered schema")
		}
	}

	user := op.Context.User
	if user == "" {
		user = "system"
	}
	txMgr.CurrentUser = user

	outcomes := make([]stepOutcome, 0, len(cmd.Steps))
	lastRecord, lastName := "", ""
	lastBusinessScope := op.Context.BusinessScope
	stepNames := map[string]string{}
	pinnedNow := time.Now().UTC()
	computed, computeErr := evalCompute(cmd.Compute, input)
	if computeErr != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, computeErr.Error())
	}
	fieldReader := k.stepFieldReader(txMgr, reg, op, cmd, stepNames)
	if cerr := k.checkCommandGuards(run, op, cmd, refEnv{input: input, steps: stepNames, fieldReader: fieldReader, computed: computed, now: pinnedNow, actor: user}); cerr != nil { //nolint:contextcheck // the context travels in txMgr.Context
		auditFailure(cerr.Type)
		return nil, cerr
	}
	rows, rowsErr := k.loadGuardRows(txMgr, reg, op, cmd, stepNames)
	if rowsErr != nil {
		auditFailure(rowsErr.Type)
		return nil, rowsErr
	}
	instances, expandErr := expandCommandSteps(cmd.Steps, refEnv{input: input, steps: stepNames, fieldReader: fieldReader, computed: computed, now: pinnedNow, actor: user}, rows)
	if expandErr != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, expandErr.Error())
	}

	for _, instance := range instances {
		i, step := instance.index, instance.step
		env := refEnv{input: input, item: instance.item, steps: stepNames, fieldReader: fieldReader, computed: computed, now: pinnedNow, actor: user}
		stepOp, scopeErr := operationForCommandStep(op, cmd, step)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if cmd.CrossEntity != nil {
			txMgr.BusinessScope = stepOp.Context.BusinessScope
		}
		lastBusinessScope = stepOp.Context.BusinessScope
		if step.Ensure != nil {
			existing, ensureErr := ensureExisting(txMgr, reg, step.Ensure, stepEnv{env, i}, scopeForOperation(reg, stepOp, step.Ensure.Record, "write")) //nolint:contextcheck // the context travels in txMgr.Context
			if ensureErr != nil {
				return nil, ensureErr
			}
			if existing != "" {
				// Finding a record is not a write: no outcome, and the idempotency
				// receipt is still claimed by the first step that writes.
				if step.As != "" {
					stepNames[step.As] = existing
				}
				continue
			}
		}
		switch {
		case step.createSpec() != nil:
			create := step.createSpec()
			dt := reg.Get(create.Record)
			if dt == nil {
				return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("command %q: record %q not found", def.Name, create.Record))
			}
			doc := doctype.NewDocument(dt.Name)
			for field, ref := range create.Values {
				v, rerr := resolveEnvRef(ref, env)
				if errors.Is(rerr, errRefAbsent) {
					continue
				}
				if rerr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
				}
				if tf := dt.GetField(field); tf != nil && tf.Fieldtype == "Table" {
					if rows, ok := v.([]any); ok {
						v = childDocuments(tf.Options, rows)
					}
				}
				doc.Set(field, v)
			}
			if err := dt.BindBusinessScope(doc, txMgr.BusinessScope, true, nil); err != nil {
				return nil, contract.NewError(contract.CodePermissionDenied, err.Error())
			}
			setDefaultValues(dt, doc)
			if err := txMgr.RunHooksForValidate(dt, doc, nil); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if verrs := validateOperationDocument(txMgr, dt, doc, reg, documentCheck{Old: nil, References: op.Context.BusinessReferenceValidator, Snapshots: op.Context.HistoricalSnapshotValidator}); verrs.HasErrors() { //nolint:contextcheck // the context travels in txMgr.Context
				return nil, validationContractError(verrs)
			}
			if err := txMgr.PrepareInsert(dt, doc); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if op.Context.IdempotencyKey != "" && def.IdempotentByKey && len(outcomes) == 0 {
				if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
					return nil, cerr
				}
			}
			owner := op.Context.Owner
			if owner == "" {
				owner = user
			}
			if err := txMgr.InsertInTx(dbTx, dt, doc, owner, user); err != nil {
				return nil, wrapORMError(err)
			}
			if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterInsert, doc, nil}); hookErr != nil {
				return nil, hookErr
			}
			if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, nil}); hookErr != nil {
				return nil, hookErr
			}
			doc.IsNew = false
			completeTx := *txMgr
			completeTx.ScriptMutationExecutor = nil
			completeTx.PostCommitHooks = nil
			completeTx.Transaction = nil
			completeTx.SkipHookScripts = nil
			txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteInsert(dt, doc) })
			outcomes = append(outcomes, stepOutcome{Record: dt.Name, Name: doc.Name, Created: true})
			lastRecord, lastName = dt.Name, doc.Name
			if step.As != "" {
				stepNames[step.As] = doc.Name
			}

		case step.Update != nil:
			dt := reg.Get(step.Update.Record)
			if dt == nil {
				return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("command %q: record %q not found", def.Name, step.Update.Record))
			}
			scope := scopeForOperation(reg, stepOp, dt.Name, "write")
			targetName, targetErr := resolveUpdateTarget(txMgr, dt, step.Update, stepEnv{env, i}, scope) //nolint:contextcheck // the context travels in txMgr.Context
			if targetErr != nil {
				return nil, targetErr
			}
			if len(step.Update.Adjust) > 0 {
				if err := lockRecordRow(dbTx, k.dialect(), dt, targetName); err != nil {
					if err == sql.ErrNoRows {
						return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("record %q not found", targetName))
					}
					return nil, contract.NewError(contract.CodeDependencyUnavailable, "lock document failed")
				}
			}
			oldDoc, err := txMgr.GetDocWithScope(dt, targetName, scope)
			if err != nil {
				if err == sql.ErrNoRows || strings.Contains(err.Error(), orm.ErrNotFound.Error()) {
					return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("record %q not found", targetName))
				}
				return nil, contract.NewError(contract.CodeDependencyUnavailable, "load document failed")
			}
			if ev := expectedVersionFrom(op); ev != "" {
				prior, perr := readRowVersion(dbTx, k.dialect(), dt.RawTableName(), targetName)
				if perr != nil {
					return nil, contract.NewError(contract.CodeNotFound, "document not found")
				}
				if prior != ev {
					auditFailure(ErrStaleVersion.Type)
					return nil, ErrStaleVersion
				}
			}
			doc := cloneDoc(oldDoc)
			for field, ref := range step.Update.Values {
				v, rerr := resolveEnvRef(ref, env)
				if errors.Is(rerr, errRefAbsent) {
					continue
				}
				if rerr != nil {
					return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
				}
				doc.Set(field, v)
			}
			if adjustErr := applyAdjustments(dt, oldDoc, doc, step.Update.Adjust, stepEnv{env, i}); adjustErr != nil {
				return nil, adjustErr
			}
			doc.IsNew = false
			if verrs := validateOperationDocument(txMgr, dt, doc, reg, documentCheck{Old: oldDoc, References: op.Context.BusinessReferenceValidator, Snapshots: op.Context.HistoricalSnapshotValidator}); verrs.HasErrors() { //nolint:contextcheck // the context travels in txMgr.Context
				return nil, validationContractError(verrs)
			}
			if err := txMgr.PrepareSave(dt, doc, oldDoc); err != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
			}
			if op.Context.IdempotencyKey != "" && def.IdempotentByKey && len(outcomes) == 0 {
				if cerr := k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload)); cerr != nil {
					return nil, cerr
				}
			}
			if err := txMgr.SaveInTxWithScope(dbTx, dt, doc, user, scope, oldDoc); err != nil {
				return nil, wrapExpectedVersionORMError(err, op, k.dialect())
			}
			if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
				return nil, hookErr
			}
			completeTx := *txMgr
			completeTx.ScriptMutationExecutor = nil
			completeTx.PostCommitHooks = nil
			completeTx.Transaction = nil
			completeTx.SkipHookScripts = nil
			txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteSave(dt, doc, oldDoc) })
			outcomes = append(outcomes, stepOutcome{Record: dt.Name, Name: targetName, Created: false})
			lastRecord, lastName = dt.Name, targetName
			if step.As != "" {
				stepNames[step.As] = targetName
			}
		}
	}

	result := map[string]any{"command": def.Name, "steps": outcomes}
	if len(cmd.Output) > 0 {
		outEnv := refEnv{input: input, steps: stepNames, fieldReader: fieldReader, computed: computed, now: pinnedNow, actor: user}
		out := make(map[string]any, len(cmd.Output))
		for key, ref := range cmd.Output {
			v, rerr := resolveEnvRef(ref, outEnv)
			if rerr != nil {
				return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("output %q: %v", key, rerr))
			}
			out[key] = v
		}
		result["output"] = out
	}
	raw, _ := json.Marshal(result)
	if cmd.ModuleOwnerID != "" {
		if err := k.CommandSchemas.Validate(cmd.OutputSchema, raw); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "owner command output failed its registered schema")
		}
	}

	// Emit declared events through the outbox INSIDE the transaction so a
	// committed command's events are guaranteed durable (spec §19, §41).
	eventData, _ := json.Marshal(map[string]any{"command": def.Name, "steps": outcomes})
	for _, evtType := range cmd.Emits {
		env := contract.EventEnvelope{
			Type:          evtType,
			Source:        "kora.kernel",
			Site:          op.Context.Site,
			BusinessScope: lastBusinessScope,
			AggregateType: lastRecord,
			AggregateID:   lastName,
			CorrelationID: op.Context.CorrelationID,
			CausationID:   opID,
			Actor:         op.Context.Actor,
			Data:          eventData,
		}
		if k.Outbox == nil {
			break // no provider wired; skip silently only in tests — production wiring always sets Outbox
		}
		if err := k.Outbox.Append(ctx, dbTx, env); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "recording command event failed")
		}
	}
	for _, eventDef := range cmd.BusinessEvents {
		if k.Outbox == nil || k.CommandSchemas == nil {
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "business event outbox or schema registry is unavailable")
		}
		if !k.CommandSchemas.Registered(eventDef.PayloadSchema) {
			return nil, contract.NewError(contract.CodeValidationFailed, "business event payload schema is not registered")
		}
		if err := k.CommandSchemas.Validate(eventDef.PayloadSchema, op.Payload); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "business event payload failed its registered schema")
		}
		execution := op.Context.ModuleExecution
		if execution == nil || !execution.Verified() || execution.OwnerModuleID != eventDef.OwnerModuleID {
			return nil, contract.NewError(contract.CodePermissionDenied, "business event requires verified owner execution")
		}
		if _, err := contract.AppendBusinessEvent(ctx, k.Outbox, dbTx, k.CommandSchemas, *execution, eventDef, eventDef.PayloadSchema, op.Payload, op.Context.Site, lastRecord, lastName, op.Context.CorrelationID, opID); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "recording owned business event failed: "+err.Error())
		}
	}

	row := k.buildAuditRow(op, def, opID, auditOutcome{Doctype: lastRecord, DocName: lastName, Status: contract.StatusCompleted})
	row.Command = def.Name
	if _, aerr := k.writeAudit(dbTx, row); aerr != nil {
		return nil, contract.NewError(contract.CodeInternal, "audit write failed")
	}

	if op.Context.IdempotencyKey != "" && def.IdempotentByKey {
		if cerr := k.storeReceiptResult(dbTx, op, contract.CommandResult{
			OperationID: opID, CorrelationID: op.Context.CorrelationID,
			Status: contract.StatusCompleted, Data: raw,
		}); cerr != nil {
			return nil, cerr
		}
	}

	postCommitHooks := append([]func(){}, txMgr.PostCommitHooks...)
	complete := func() {
		for _, hook := range postCommitHooks {
			hook()
		}
	}
	if ownsTransaction {
		if err := dbTx.Commit(); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "commit failed")
		}
		txMgr.ScriptMutationExecutor = nil
		txMgr.PostCommitHooks = nil
		txMgr.Transaction = nil
		complete()
	} else if afterCommit != nil {
		*afterCommit = complete
	}

	return raw, nil
}

// decodeInput extracts {"data": {...}} from the operation payload.
func decodeInput(raw json.RawMessage) (map[string]any, *contract.Error) {
	var wrapper struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "payload must be {\"data\":{...}}")
	}
	if wrapper.Data == nil {
		wrapper.Data = map[string]any{}
	}
	return wrapper.Data, nil
}

// resolveRef returns literal values as-is and resolves "$input.path" refs.
func resolveRef(ref string, input map[string]any) (any, error) {
	if !strings.HasPrefix(ref, "$input.") {
		return ref, nil
	}
	return walkRef(ref, input, strings.TrimPrefix(ref, "$input."))
}

func resolveStringRef(ref string, input map[string]any) (string, error) {
	v, err := resolveRef(ref, input)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("reference %q must resolve to a string", ref)
	}
	return s, nil
}

// readRowVersion reads the canonical version token for optimistic concurrency.
func readRowVersion(dbTx *sql.Tx, dialect db.Dialect, table, name string) (string, error) {
	var modified any
	query := fmt.Sprintf("SELECT modified FROM %s WHERE name = ?", dialect.QuoteIdent(table))
	if err := dbTx.QueryRow(db.Rebind(dialect, query), name).Scan(&modified); err != nil {
		return "", err
	}
	switch value := modified.(type) {
	case time.Time:
		return CanonicalVersion(value), nil
	case string:
		parsed, err := parseVersionTimestamp(value)
		if err != nil {
			return "", err
		}
		return CanonicalVersion(parsed), nil
	case []byte:
		parsed, err := parseVersionTimestamp(string(value))
		if err != nil {
			return "", err
		}
		return CanonicalVersion(parsed), nil
	default:
		return "", fmt.Errorf("unsupported modified timestamp type %T", modified)
	}
}

// childDocuments turns resolved Table rows into child documents so a command
// create step can write a parent and its child rows together.
func childDocuments(childDoctype string, rows []any) []*doctype.Document {
	children := make([]*doctype.Document, 0, len(rows))
	for _, row := range rows {
		child := doctype.NewDocument(childDoctype)
		if fields, ok := row.(map[string]any); ok {
			for name, value := range fields {
				child.Set(name, value)
			}
		}
		children = append(children, child)
	}
	return children
}
