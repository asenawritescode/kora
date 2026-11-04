package requests

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/contract"
)

// TransactionDecisionStore persists a prepared response using a transaction
// owned by the caller. Implementations must neither commit nor roll it back.
type TransactionDecisionStore interface {
	CommitDecisionInTx(context.Context, *sql.Tx, DecisionCommit) (contract.ResponseReceipt, error)
}

// PreparedSubmission is an opaque result of current request, subject, policy,
// membership and assignment authorization. CommitInTx atomically records the
// request and its assignment/history/outbox/idempotency rows.
type PreparedSubmission struct {
	store  TransactionCreateStore
	change CreateCommit
}

// CommitInTx appends the prepared request to a caller-owned transaction. The
// caller remains responsible for committing or rolling back the transaction.
func (p *PreparedSubmission) CommitInTx(ctx context.Context, tx *sql.Tx) (RequestRecord, error) {
	if p == nil || p.store == nil || tx == nil {
		return RequestRecord{}, fmt.Errorf("prepared submission requires a request store and caller transaction")
	}
	return p.store.CreateInTx(ctx, tx, p.change)
}

// PreparedResponse is an opaque result of response-time authorization. It
// binds the decision to the request revision, subject snapshot, actor and
// authorization epochs checked during preparation.
type PreparedResponse struct {
	store  TransactionDecisionStore
	change DecisionCommit
	replay *contract.ResponseReceipt
}

// ResponseContext is the server-authorized snapshot a composition layer may
// use to select a registered local domain action. It contains no caller-chosen
// command or transport identity.
type ResponseContext struct {
	Tenant        contract.TenantID
	RequestID     contract.RequestID
	Kind          contract.RequestKind
	PolicyID      string
	Subject       *contract.SubjectRef
	SubjectDigest string
}

// Context returns a copied immutable view of the request bound to this
// prepared response. Replays have no action context and must not execute work.
func (p *PreparedResponse) Context() (ResponseContext, bool) {
	if p == nil || p.replay != nil || p.change.Request.ID == "" {
		return ResponseContext{}, false
	}
	var subject *contract.SubjectRef
	if p.change.Request.Subject != nil {
		copy := *p.change.Request.Subject
		subject = &copy
	}
	return ResponseContext{Tenant: p.change.Request.Tenant, RequestID: p.change.Request.ID, Kind: p.change.Request.Kind,
		PolicyID: p.change.Request.PolicyID, Subject: subject, SubjectDigest: p.change.Request.SubjectDigest}, true
}

// ReplayReceipt reports a previously committed result after the service has
// rechecked that the current execution can still see the request. Coordinators
// must return this receipt without executing any domain effect.
func (p *PreparedResponse) ReplayReceipt() (contract.ResponseReceipt, bool) {
	if p == nil || p.replay == nil {
		return contract.ResponseReceipt{}, false
	}
	return *p.replay, true
}

// CommitInTx records the response in the caller's transaction. The caller
// remains responsible for committing or rolling back that transaction.
func (p *PreparedResponse) CommitInTx(ctx context.Context, tx *sql.Tx) (contract.ResponseReceipt, error) {
	if receipt, replay := p.ReplayReceipt(); replay {
		return receipt, nil
	}
	if p == nil || p.store == nil || tx == nil {
		return contract.ResponseReceipt{}, fmt.Errorf("prepared response requires a request store and caller transaction")
	}
	return p.store.CommitDecisionInTx(ctx, tx, p.change)
}
