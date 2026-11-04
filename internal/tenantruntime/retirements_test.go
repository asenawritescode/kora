package tenantruntime

import (
	"context"
	"errors"
	"testing"
)

func TestRetirementsBoundAndWaitForCapacity(t *testing.T) {
	r, err := NewRetirements(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error = %v, want canceled", err)
	}
	if got := r.InUse(); got != 1 {
		t.Fatalf("slots in use = %d, want 1", got)
	}
	r.Release()
	if err := r.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	r.Release()
}

func TestRetirementsRejectZeroLimit(t *testing.T) {
	if _, err := NewRetirements(0); !errors.Is(err, ErrRetirementLimit) {
		t.Fatalf("NewRetirements error = %v", err)
	}
}
