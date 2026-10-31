package tenantruntime

import (
	"context"
	"errors"
)

var ErrRetirementLimit = errors.New("retiring tenant generation limit must be positive")

// Retirements bounds generations whose requests or workers are still draining.
// A slot is acquired before removing/replacing a runtime and released only
// after its resources are closed.
type Retirements struct{ slots chan struct{} }

func NewRetirements(limit uint16) (*Retirements, error) {
	if limit == 0 {
		return nil, ErrRetirementLimit
	}
	return &Retirements{slots: make(chan struct{}, limit)}, nil
}

func (r *Retirements) Acquire(ctx context.Context) error {
	if r == nil || r.slots == nil {
		return ErrRetirementLimit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case r.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Retirements) Release() {
	if r == nil || r.slots == nil {
		return
	}
	<-r.slots
}

func (r *Retirements) InUse() int {
	if r == nil || r.slots == nil {
		return 0
	}
	return len(r.slots)
}
