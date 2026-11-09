package script

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
)

const (
	MaxAsyncHookAttempts     uint8 = 5
	MaxAsyncHookBatch              = 32
	MaxAsyncHookPayloadBytes       = 1 << 20
	MaxAsyncHookTimeout            = 4 * time.Minute
)

// AsyncHookInvocation freezes one document lifecycle script selected inside
// the source operation transaction. It is not a public wire contract.
type AsyncHookInvocation struct {
	TenantID        contract.TenantID     `json:"tenant_id"`
	Site            string                `json:"site"`
	SourceOperation contract.OperationID  `json:"source_operation_id"`
	ConfigRevision  uint64                `json:"config_revision,omitempty"`
	Sequence        uint16                `json:"sequence"`
	Doctype         string                `json:"doctype"`
	Event           Event                 `json:"event"`
	Record          Record                `json:"record"`
	Document        json.RawMessage       `json:"document"`
	OldDocument     json.RawMessage       `json:"old_document,omitempty"`
	Actor           contract.ActorContext `json:"actor"`
	Grants          []policy.Grant        `json:"grants,omitempty"`
}

// AsyncHookClaim is a fenced lease. The database lease, not LeaseUntil's
// process-clock estimate, is authoritative.
type AsyncHookClaim struct {
	Invocation AsyncHookInvocation
	Owner      string
	Fence      uint64
	Attempt    uint8
	LeaseUntil time.Time
}

type AsyncHookOutcome struct {
	Success    bool
	Retry      bool
	Uncertain  bool
	Error      string
	RetryAfter time.Duration
}

// AsyncHookInvocationStore defines the source-transaction append and
// recoverable worker boundary. A started external effect is durable before
// the provider sends the request; an expired such claim must become uncertain.
type AsyncHookInvocationStore interface {
	AppendAsyncHookTx(context.Context, *sql.Tx, AsyncHookInvocation) error
	ClaimAsyncHooks(context.Context, contract.TenantID, string, uint16, time.Duration) ([]AsyncHookClaim, error)
	BeginAsyncHook(context.Context, AsyncHookClaim) (*sql.Tx, error)
	MarkAsyncHookExternalEffect(context.Context, AsyncHookClaim) error
	SucceedAsyncHookTx(context.Context, *sql.Tx, AsyncHookClaim) error
	FinishAsyncHook(context.Context, AsyncHookClaim, AsyncHookOutcome) error
}
