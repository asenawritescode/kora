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
	ToLocation  string       `json:"to_location,omitempty"`
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
	if m.Kind == MovementReserve || m.Kind == MovementRelease {
		if m.Quantity < 0 {
			return fmt.Errorf("%s quantity must be positive", m.Kind)
		}
		available := l.balanceLocked(m.Item, m.Location) - l.reservedLocked(m.Item, m.Location)
		if m.Kind == MovementReserve && !l.policy.AllowNegative && available < m.Quantity {
			return fmt.Errorf("reservation would exceed available stock for %s at %s", m.Item, m.Location)
		}
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
		if m.Item == item && m.Location == location && m.Kind != MovementReserve && m.Kind != MovementRelease {
			total += m.Quantity
		}
	}
	return total
}

func (l *Ledger) reservedLocked(item, location string) int64 {
	var total int64
	for _, m := range l.moves {
		if m.Item != item || m.Location != location {
			continue
		}
		if m.Kind == MovementReserve {
			total += m.Quantity
		}
		if m.Kind == MovementRelease {
			total -= m.Quantity
		}
	}
	return total
}

// Reserved reports stock held for pending work without changing physical stock.
func (l *Ledger) Reserved(item, location string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.reservedLocked(item, location)
}

// Available reports physical stock that is not reserved.
func (l *Ledger) Available(item, location string) int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.balanceLocked(item, location) - l.reservedLocked(item, location)
}

func (l *Ledger) Reserve(id, item, location string, quantity int64, actor, reason string) error {
	return l.Append(Movement{ID: id, Item: item, Location: location, Quantity: quantity, Kind: MovementReserve, Actor: actor, Reason: reason})
}

func (l *Ledger) Release(id, item, location string, quantity int64, actor, reason string) error {
	if quantity <= 0 {
		return fmt.Errorf("release quantity must be positive")
	}
	l.mu.RLock()
	reserved := l.reservedLocked(item, location)
	l.mu.RUnlock()
	if reserved < quantity {
		return fmt.Errorf("release exceeds reserved stock for %s at %s", item, location)
	}
	return l.Append(Movement{ID: id, Item: item, Location: location, Quantity: quantity, Kind: MovementRelease, Actor: actor, Reason: reason})
}

// Transfer records both sides of a location change. The pair is append-only;
// callers can reverse either side through compensating movements.
func (l *Ledger) Transfer(sourceID, destinationID, item, from, to string, quantity int64, actor, reason string) error {
	if quantity <= 0 || from == "" || to == "" || from == to {
		return fmt.Errorf("transfer requires positive quantity and distinct locations")
	}
	if err := l.Append(Movement{ID: sourceID, Item: item, Location: from, Quantity: -quantity, Kind: MovementTransfer, ToLocation: to, Actor: actor, Reason: reason}); err != nil {
		return err
	}
	if err := l.Append(Movement{ID: destinationID, Item: item, Location: to, Quantity: quantity, Kind: MovementTransfer, ToLocation: from, Actor: actor, Reason: reason}); err != nil {
		_, _ = l.Reverse(sourceID, sourceID+"-reversal", actor, "compensate incomplete transfer")
		return err
	}
	return nil
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
