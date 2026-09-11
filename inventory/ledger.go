// Package inventory is the reference domain package for Kora's organizational
// runtime. Stock is a projection of an append-only movement ledger; callers
// cannot mutate a balance directly.
package inventory

import (
	"fmt"
	"sync"
	"time"
)

type MovementKind string

const (
	MovementReceive  MovementKind = "receive"
	MovementIssue    MovementKind = "issue"
	MovementTransfer MovementKind = "transfer"
	MovementAdjust   MovementKind = "adjust"
	MovementReserve  MovementKind = "reserve"
	MovementRelease  MovementKind = "release"
)

type Movement struct {
	ID          string       `json:"id"`
	Item        string       `json:"item"`
	Location    string       `json:"location"`
	Quantity    int64        `json:"quantity"`
	Kind        MovementKind `json:"kind"`
	Actor       string       `json:"actor"`
	Reason      string       `json:"reason"`
	OccurredAt  time.Time    `json:"occurred_at"`
	Compensates string       `json:"compensates,omitempty"`
}

type Policy struct{ AllowNegative bool }

type Ledger struct {
	mu     sync.RWMutex
	policy Policy
	moves  []Movement
	byID   map[string]Movement
}

func NewLedger(policy Policy) *Ledger { return &Ledger{policy: policy, byID: map[string]Movement{}} }

func (l *Ledger) Append(m Movement) error {
	if m.ID == "" || m.Item == "" || m.Location == "" || m.Actor == "" || m.Reason == "" || m.Kind == "" || m.Quantity == 0 {
		return fmt.Errorf("movement requires id, item, location, non-zero quantity, kind, actor, and reason")
	}
	if m.OccurredAt.IsZero() {
		m.OccurredAt = time.Now().UTC()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.byID[m.ID]; exists {
		return fmt.Errorf("movement %q already exists", m.ID)
	}
	if !l.policy.AllowNegative && l.balanceLocked(m.Item, m.Location)+m.Quantity < 0 {
		return fmt.Errorf("movement would create negative stock for %s at %s", m.Item, m.Location)
	}
	l.moves = append(l.moves, m)
	l.byID[m.ID] = m
	return nil
}

func (l *Ledger) Reverse(id, reversalID, actor, reason string) (Movement, error) {
	l.mu.RLock()
	original, ok := l.byID[id]
	l.mu.RUnlock()
	if !ok {
		return Movement{}, fmt.Errorf("movement %q not found", id)
	}
	reversal := Movement{ID: reversalID, Item: original.Item, Location: original.Location, Quantity: -original.Quantity, Kind: MovementAdjust, Actor: actor, Reason: reason, Compensates: original.ID}
	if err := l.Append(reversal); err != nil {
		return Movement{}, err
	}
	return reversal, nil
}

func (l *Ledger) Balance(item, location string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balanceLocked(item, location)
}

func (l *Ledger) balanceLocked(item, location string) int64 {
	var total int64
	for _, m := range l.moves {
		if m.Item == item && m.Location == location {
			total += m.Quantity
		}
	}
	return total
}

func (l *Ledger) Movements() []Movement {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Movement, len(l.moves))
	copy(out, l.moves)
	return out
}

type LowStockAlert struct {
	Item      string `json:"item"`
	Location  string `json:"location"`
	Balance   int64  `json:"balance"`
	Threshold int64  `json:"threshold"`
}

func (l *Ledger) LowStock(item, location string, threshold int64) (LowStockAlert, bool) {
	balance := l.Balance(item, location)
	alert := LowStockAlert{Item: item, Location: location, Balance: balance, Threshold: threshold}
	return alert, balance < threshold
}
