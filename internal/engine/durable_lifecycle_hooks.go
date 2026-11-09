package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/secret"
)

const (
	durableLifecycleHookPoll       = time.Second
	durableLifecycleHookLease      = 6 * time.Minute
	durableLifecycleHookMaxTimeout = 4 * time.Minute
)

func boundedWorkflowScriptError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

// hookWorkAdmission gates the database work of a lifecycle hook pass.
type hookWorkAdmission interface {
	Acquire(context.Context) (func(), error)
}

// LifecycleHookWorker holds the dependencies of one tenant's durable lifecycle
// hook worker. An omitted Outbox, Secrets, HTTPAllowlist or Admission is
// treated as absent.
type LifecycleHookWorker struct {
	Runner        script.Runner
	Store         *script.Store
	DB            *sql.DB
	Registry      *doctype.Registry
	Outbox        outbox.Writer
	TenantID      string
	Site          string
	DBType        string
	Secrets       *secret.Store
	HTTPAllowlist []string
	Admission     hookWorkAdmission
}

// RunDurableLifecycleHookWorker is owned by one tenant runtime generation.
// Its SQL rows are the recovery authority; process cancellation only stops
// new claims and causes an in-flight transaction to roll back.
func RunDurableLifecycleHookWorker(ctx context.Context, w LifecycleHookWorker) {
	if ctx == nil || w.Runner == nil || w.Store == nil || w.DB == nil || w.Registry == nil || w.TenantID == "" || w.Site == "" {
		return
	}
	tenant, err := contract.ParseTenantID(w.TenantID)
	if err != nil {
		slog.Error("durable lifecycle hook worker has invalid tenant", "site", w.Site, "error", err)
		return
	}
	pass := &lifecycleHookPass{LifecycleHookWorker: w, dialect: kdb.Resolve(w.DBType), tenant: tenant, owner: "doc-hook-" + contract.NewOperationID()}
	ticker := time.NewTicker(durableLifecycleHookPoll)
	defer ticker.Stop()
	for ctx.Err() == nil {
		worked, workErr := processOneDurableLifecycleHook(ctx, pass)
		if workErr != nil && ctx.Err() == nil {
			slog.Warn("durable lifecycle hook worker pass failed", "site", w.Site, "error", workErr)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// lifecycleHookPass is one claim attempt of a tenant's durable lifecycle hook
// worker: the worker's dependencies plus the values resolved once per worker.
type lifecycleHookPass struct {
	LifecycleHookWorker
	dialect kdb.Dialect
	tenant  contract.TenantID
	owner   string
}

func processOneDurableLifecycleHook(ctx context.Context, w *lifecycleHookPass) (bool, error) {
	release, err := acquireAsyncHookWork(ctx, w.Admission)
	if err != nil {
		return false, err
	}
	defer release()
	claims, err := w.Store.ClaimAsyncHooks(ctx, w.tenant, w.owner, 1, durableLifecycleHookLease)
	if err != nil {
		return false, err
	}
	if len(claims) == 0 {
		return false, nil
	}
	claim := claims[0]
	invocation := claim.Invocation
	if invocation.Site != w.Site || invocation.Actor.Site != w.Site || invocation.TenantID != w.tenant || !invocation.Actor.Authenticated() || invocation.SourceOperation == "" || invocation.ConfigRevision == 0 || invocation.Sequence == 0 || invocation.Doctype == "" || invocation.Record.Name == "" || invocation.Record.ScriptType != script.TypeDocEvent || invocation.Record.Script == "" || invocation.Record.TimeoutMs <= 0 || invocation.Record.TimeoutMs > int(script.MaxAsyncHookTimeout/time.Millisecond) || !script.IsAfterEvent(invocation.Event) {
		return true, finishDurableLifecycleHook(ctx, w.Store, claim, script.AsyncHookOutcome{Error: "persisted lifecycle hook failed identity, revision, or source validation"})
	}
	dt := w.Registry.Get(invocation.Doctype)
	if dt == nil {
		return true, finishDurableLifecycleHook(ctx, w.Store, claim, script.AsyncHookOutcome{Error: "persisted lifecycle hook doctype is no longer registered"})
	}
	var document, oldDocument map[string]any
	if err := json.Unmarshal(invocation.Document, &document); err != nil || document == nil {
		return true, finishDurableLifecycleHook(ctx, w.Store, claim, script.AsyncHookOutcome{Error: "persisted lifecycle hook document snapshot is invalid"})
	}
	if len(invocation.OldDocument) > 0 {
		if err := json.Unmarshal(invocation.OldDocument, &oldDocument); err != nil || oldDocument == nil {
			return true, finishDurableLifecycleHook(ctx, w.Store, claim, script.AsyncHookOutcome{Error: "persisted lifecycle hook previous document snapshot is invalid"})
		}
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(w.tenant, invocation.Actor, invocation.Grants)
	if err != nil {
		return true, finishDurableLifecycleHook(ctx, w.Store, claim, script.AsyncHookOutcome{Error: "persisted lifecycle hook execution identity is invalid"})
	}
	execTx, err := w.Store.BeginAsyncHook(ctx, claim)
	if err != nil {
		return true, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = execTx.Rollback()
		}
	}()
	timeout := time.Duration(invocation.Record.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	} else if timeout > durableLifecycleHookMaxTimeout {
		timeout = durableLifecycleHookMaxTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	actor := execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	tm := &orm.TxManager{
		DB: w.DB, Transaction: execTx, Registry: w.Registry, Dialect: w.dialect,
		Context: callCtx, SiteName: w.Site, CurrentUser: user, CurrentUserRoles: actor.Roles,
		Execution: execution, ScriptRunner: w.Runner, ScriptStore: w.Store,
		ScriptProvider: nil, AsyncHookInvocations: w.Store, AsyncHookTenantID: w.tenant,
		AsyncHookSourceOperation: contract.OperationID(contract.NewOperationID()), AsyncHookConfigRevision: invocation.ConfigRevision,
		SkipHookScripts: []string{invocation.Record.Name}, SuppressAsyncHookEvents: true,
	}
	tm.Outbox = w.Outbox
	operationKernel := kernel.New(w.dialect, tm.Outbox)
	tm.ScriptMutationExecutor = script.MutationExecutorFunc(func(nestedCtx context.Context, request script.MutationRequest) (json.RawMessage, error) {
		return operationKernel.ExecuteScriptMutationInTx(nestedCtx, execTx, tm, request)
	})
	provider := api.NewScriptProvider(tm, w.Registry, w.Site, w.Secrets, w.HTTPAllowlist)
	if closer, ok := provider.(interface{ Close() }); ok {
		defer closer.Close()
	}
	if scoped, ok := provider.(script.LifecycleScopedProvider); ok {
		provider = scoped.WithLifecycleHook(invocation.Doctype, invocation.Event, invocation.Record.Name)
	}
	if scoped, ok := provider.(script.MutationScopedProvider); ok {
		provider = scoped.WithMutationExecutor(tm.ScriptMutationExecutor)
	}
	journal := &durableLifecycleHookJournal{store: w.Store, claim: claim}
	if scoped, ok := provider.(script.ExternalEffectScopedProvider); ok {
		provider = scoped.WithExternalEffectJournal(journal)
	}
	record := invocation.Record
	started := time.Now()
	result, runErr := executeDurableLifecycleHookSafely(w.Runner, callCtx, script.ExecuteRequest{
		Script: record.Script, ScriptType: record.ScriptType, ScriptName: record.Name,
		DocType: invocation.Doctype, Event: invocation.Event, Document: document,
		OldDocument: oldDocument, User: user, UserRoles: append([]string(nil), actor.Roles...),
		Site: w.Site, Timeout: timeout, Provider: provider,
	})
	duration := time.Since(started)
	if result != nil && result.Duration > 0 {
		duration = result.Duration
	}
	if runErr != nil {
		_ = execTx.Rollback()
		committed = true
		outcome := script.AsyncHookOutcome{Error: boundedWorkflowScriptError(runErr), Retry: true}
		if journal.Started() {
			outcome.Retry, outcome.Uncertain = false, true
		}
		finishErr := finishDurableLifecycleHook(ctx, w.Store, claim, outcome)
		w.logExecution(invocation, user, duration, "error", outcome.Error)
		return true, finishErr
	}
	if err := w.Store.SucceedAsyncHookTx(callCtx, execTx, claim); err != nil {
		_ = execTx.Rollback()
		committed = true
		outcome := script.AsyncHookOutcome{Error: "async lifecycle hook lost its worker fence before commit", Retry: true}
		if journal.Started() {
			outcome.Retry, outcome.Uncertain = false, true
		}
		return true, finishDurableLifecycleHook(ctx, w.Store, claim, outcome)
	}
	if err := execTx.Commit(); err != nil {
		committed = true // a commit error can be ambiguous; never issue an unsafe retry here.
		if journal.Started() {
			finishErr := finishDurableLifecycleHook(ctx, w.Store, claim, script.AsyncHookOutcome{Error: "commit result is unknown after external effect", Uncertain: true})
			return true, errors.Join(fmt.Errorf("commit local async hook effects: %w", err), finishErr)
		}
		return true, fmt.Errorf("commit local async hook effects; receipt state must be reconciled: %w", err)
	}
	committed = true
	for _, afterCommit := range tm.PostCommitHooks {
		afterCommit()
	}
	tm.PostCommitHooks = nil
	w.logExecution(invocation, user, duration, "success", "")
	return true, nil
}

func executeDurableLifecycleHookSafely(runner script.Runner, ctx context.Context, request script.ExecuteRequest) (result *script.ExecuteResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("lifecycle hook panic: %v", recovered)
		}
	}()
	return runner.Execute(ctx, request)
}

type durableLifecycleHookJournal struct {
	store   script.AsyncHookInvocationStore
	claim   script.AsyncHookClaim
	once    sync.Once
	err     error
	mu      sync.Mutex
	started bool
}

func (j *durableLifecycleHookJournal) BeforeExternalEffect(ctx context.Context) error {
	if j == nil || j.store == nil {
		return errors.New("external effect journal is unavailable")
	}
	j.once.Do(func() {
		j.err = j.store.MarkAsyncHookExternalEffect(ctx, j.claim)
		j.mu.Lock()
		j.started = j.err == nil
		j.mu.Unlock()
	})
	return j.err
}

func (j *durableLifecycleHookJournal) Started() bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.started
}

func finishDurableLifecycleHook(ctx context.Context, store script.AsyncHookInvocationStore, claim script.AsyncHookClaim, outcome script.AsyncHookOutcome) error {
	finishCtx := ctx
	cancel := func() {}
	if ctx == nil || ctx.Err() != nil {
		finishCtx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	}
	defer cancel()
	if outcome.Retry && !outcome.Uncertain {
		shift := claim.Attempt - 1
		if shift > 5 {
			shift = 5
		}
		outcome.RetryAfter = time.Second * time.Duration(1<<shift)
	}
	return store.FinishAsyncHook(finishCtx, claim, outcome)
}

func (w *lifecycleHookPass) logExecution(invocation script.AsyncHookInvocation, user string, duration time.Duration, status, message string) {
	if w.Store == nil {
		return
	}
	if err := w.Store.LogExecution(w.Site, invocation.Record, invocation.Doctype, lifecycleHookDocumentName(invocation.Document), invocation.Event, user, int(duration.Milliseconds()), status, message); err != nil {
		slog.Warn("durable lifecycle hook execution log failed", "site", w.Site, "script", invocation.Record.Name, "error", err)
	}
}

func lifecycleHookDocumentName(raw json.RawMessage) string {
	var document struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &document) != nil {
		return ""
	}
	return document.Name
}
