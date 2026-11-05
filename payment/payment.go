// Package payment models payment intents, providers and the request/confirm
// service. Persistence and transport live elsewhere; Service works over Store.
package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
)

// State is the lifecycle state of an Intent. Only pending moves on.
type State string

const (
	StatePending   State = "pending"
	StateConfirmed State = "confirmed"
	StateFailed    State = "failed"
)

// Intent is a request to collect a payment.
type Intent struct {
	ID                string
	Provider          string
	State             State
	Amount            decimal.Decimal
	Currency          string
	Contact           string
	SourceDoctype     string
	IdempotencyKey    string
	PayloadHash       string
	ProviderRequestID string
	ProviderReference string
	FailureReason     string
	Actor             string
	ConsumedBy        string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RequestInput is the caller payload for Service.Request.
type RequestInput struct {
	Provider       string
	Amount         decimal.Decimal
	Currency       string
	Contact        string
	SourceDoctype  string
	IdempotencyKey string
}

// Confirmation is an asynchronous provider result.
type Confirmation struct {
	Provider          string
	ProviderRequestID string
	Success           bool
	ProviderReference string
	FailureReason     string
}

// ConfirmResult reports how a Confirmation was handled.
type ConfirmResult string

const (
	ConfirmApplied   ConfirmResult = "applied"
	ConfirmDuplicate ConfirmResult = "duplicate"
	ConfirmConflict  ConfirmResult = "conflict"
)

// Outcome is a provider's immediate answer to a request.
type Outcome struct {
	State             State
	ProviderRequestID string
	ProviderReference string
	FailureReason     string
}

// Provider initiates payments with an external or local system.
type Provider interface {
	Name() string
	Request(ctx context.Context, in Intent) (Outcome, error)
}

// Registry holds providers by name.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Provider
}

// NewRegistry returns a registry with the cash provider registered.
func NewRegistry() *Registry {
	r := &Registry{m: map[string]Provider{}}
	_ = r.Register(Cash{})
	return r
}

// Register adds p; a duplicate name is an error.
func (r *Registry) Register(p Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[p.Name()]; ok {
		return fmt.Errorf("payment: provider %q already registered", p.Name())
	}
	r.m[p.Name()] = p
	return nil
}

// Get returns the named provider.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.m[name]
	return p, ok
}

// Cash confirms immediately; it is the default provider.
type Cash struct{}

func (Cash) Name() string { return "cash" }

func (Cash) Request(_ context.Context, in Intent) (Outcome, error) {
	return Outcome{State: StateConfirmed, ProviderReference: in.ID}, nil
}

// ErrNotFound is returned by Store when no intent matches.
var ErrNotFound = errors.New("payment: intent not found")

// Store persists intents. Transition must only update a pending intent and
// report whether it did.
type Store interface {
	Create(ctx context.Context, site string, in Intent) error
	Get(ctx context.Context, site, id string) (Intent, error)
	GetByKey(ctx context.Context, site, key string) (Intent, error)
	GetByProviderRequest(ctx context.Context, site, provider, reqID string) (Intent, error)
	Transition(ctx context.Context, site, id string, o Outcome) (bool, error)
}

// Service coordinates providers and the store.
type Service struct {
	Store     Store
	Providers *Registry
	Now       func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func payloadHash(in RequestInput) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s",
		in.Provider, in.Amount.String(), in.Currency, in.Contact, in.SourceDoctype)))
	return hex.EncodeToString(sum[:])
}

// Request creates an intent, idempotent on (site, key).
func (s *Service) Request(ctx context.Context, site, actor string, in RequestInput) (Intent, error) {
	if in.IdempotencyKey == "" {
		return Intent{}, contract.NewError(contract.CodeValidationFailed, "idempotency key required")
	}
	if !in.Amount.IsPositive() {
		return Intent{}, contract.NewError(contract.CodeValidationFailed, "amount must be positive")
	}
	p, ok := s.Providers.Get(in.Provider)
	if !ok {
		return Intent{}, contract.NewError(contract.CodeValidationFailed, "unknown payment provider")
	}
	hash := payloadHash(in)
	if prev, err := s.Store.GetByKey(ctx, site, in.IdempotencyKey); err == nil {
		if prev.PayloadHash != hash {
			return Intent{}, contract.NewError(contract.CodeConflict, "idempotency key reused with a different payload")
		}
		return prev, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Intent{}, err
	}
	now := s.now()
	it := Intent{
		ID: ulid.Make().String(), Provider: in.Provider, State: StatePending,
		Amount: in.Amount, Currency: in.Currency, Contact: in.Contact,
		SourceDoctype: in.SourceDoctype, IdempotencyKey: in.IdempotencyKey,
		PayloadHash: hash, Actor: actor, CreatedAt: now, UpdatedAt: now,
	}
	out, err := p.Request(ctx, it)
	if err != nil {
		out = Outcome{State: StateFailed, FailureReason: err.Error()}
	}
	if out.State == "" {
		out.State = StatePending
	}
	it.State, it.ProviderRequestID = out.State, out.ProviderRequestID
	it.ProviderReference, it.FailureReason = out.ProviderReference, out.FailureReason
	if err := s.Store.Create(ctx, site, it); err != nil {
		return Intent{}, err
	}
	return it, nil
}

// Confirm applies an asynchronous provider result exactly once.
func (s *Service) Confirm(ctx context.Context, site string, c Confirmation) (Intent, ConfirmResult, error) {
	it, err := s.Store.GetByProviderRequest(ctx, site, c.Provider, c.ProviderRequestID)
	if err != nil {
		return Intent{}, "", err
	}
	want := StateFailed
	if c.Success {
		want = StateConfirmed
	}
	if it.State != StatePending {
		if it.State == want {
			return it, ConfirmDuplicate, nil
		}
		return it, ConfirmConflict, nil
	}
	o := Outcome{State: want, ProviderRequestID: c.ProviderRequestID,
		ProviderReference: c.ProviderReference, FailureReason: c.FailureReason}
	ok, err := s.Store.Transition(ctx, site, it.ID, o)
	if err != nil {
		return Intent{}, "", err
	}
	cur, err := s.Store.Get(ctx, site, it.ID)
	if err != nil {
		return Intent{}, "", err
	}
	switch {
	case ok:
		return cur, ConfirmApplied, nil
	case cur.State == want:
		return cur, ConfirmDuplicate, nil
	}
	return cur, ConfirmConflict, nil
}

// Get returns one intent.
func (s *Service) Get(ctx context.Context, site, id string) (Intent, error) {
	it, err := s.Store.Get(ctx, site, id)
	if errors.Is(err, ErrNotFound) {
		return Intent{}, contract.NewError(contract.CodeNotFound, "payment not found")
	}
	return it, err
}
