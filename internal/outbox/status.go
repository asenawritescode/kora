package outbox

import "fmt"

// Status is the durable state of an outbox row (RFC §8.1).
type Status string

// Outbox row states. Transitions between them are listed in transitions.
const (
	StatusPending    Status = "pending"
	StatusPublishing Status = "publishing"
	StatusPublished  Status = "published"
	StatusFailed     Status = "failed"
	// StatusBlocked marks a row whose destination refused it for capacity. A
	// blocked attempt does not consume an attempt and never becomes failed.
	StatusBlocked Status = "blocked"
)

// transitions is the complete outbox state machine. Every status update passes
// through transition and names its source state in the WHERE clause, so a stale
// worker cannot move a row it no longer owns. publishing to publishing is a
// lease takeover of a claim whose lease expired.
var transitions = map[Status][]Status{
	StatusPending:    {StatusPublishing},
	StatusPublishing: {StatusPublishing, StatusPublished, StatusPending, StatusFailed, StatusBlocked},
	StatusFailed:     {StatusPending},
	StatusBlocked:    {StatusPending},
}

func (s Status) canMoveTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// transition rejects a status change outside the state machine before any SQL
// update runs.
func transition(from, to Status) error {
	if !from.canMoveTo(to) {
		return fmt.Errorf("outbox: illegal status transition %s -> %s", from, to)
	}
	return nil
}
