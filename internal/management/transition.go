package management

import (
	"slices"

	"github.com/asenawritescode/kora/contract"
)

// Move names the Store method that changes an operation's state.
type Move string

const (
	// MoveClaimDue fences a due operation for a worker: a requested operation,
	// or a running one whose lease expired (reclaimed with a new owner and fence).
	MoveClaimDue Move = "ClaimDue"
	// MoveRenew extends the lease of the claim that owns a running operation.
	MoveRenew Move = "Renew"
	// MoveRelease returns an unstarted running claim to the queue.
	MoveRelease Move = "Release"
	// MoveFinish records the outcome of a running operation and drops its lease.
	MoveFinish Move = "Finish"
	// MoveRetry is the operator exit from recovery_required: the Retry*Operation
	// store methods requeue the same operation after an audited reconciliation.
	MoveRetry Move = "Retry"
)

type transition struct{ from, to contract.OperationState }

// allowed is the whole operation state machine. succeeded and failed have no
// way out; recovery_required leaves only through an operator retry. Each SQL
// guard in the store mirrors the from-states of its move here.
var allowed = map[transition][]Move{
	{contract.OperationRequested, contract.OperationRunning}:          {MoveClaimDue},
	{contract.OperationRunning, contract.OperationRunning}:            {MoveRenew, MoveClaimDue},
	{contract.OperationRunning, contract.OperationRequested}:          {MoveRelease},
	{contract.OperationRunning, contract.OperationSucceeded}:          {MoveFinish},
	{contract.OperationRunning, contract.OperationFailed}:             {MoveFinish},
	{contract.OperationRunning, contract.OperationRecoveryRequired}:   {MoveFinish},
	{contract.OperationRecoveryRequired, contract.OperationRequested}: {MoveRetry},
}

// CanTransition reports whether any store method may move an operation from
// one state to the other.
func CanTransition(from, to contract.OperationState) bool {
	_, ok := allowed[transition{from, to}]
	return ok
}

// Allows reports whether this move may change an operation from one state to
// the other.
func (m Move) Allows(from, to contract.OperationState) bool {
	return CanTransition(from, to) && slices.Contains(allowed[transition{from, to}], m)
}
