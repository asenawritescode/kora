package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
)

type memStore struct {
	mu sync.Mutex
	m  map[string]Intent
}

func newMem() *memStore        { return &memStore{m: map[string]Intent{}} }
func k(site, id string) string { return site + "/" + id }

func (s *memStore) Create(_ context.Context, site string, in Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k(site, in.ID)] = in
	return nil
}
func (s *memStore) find(site string, f func(Intent) bool) (Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, v := range s.m {
		if len(key) > len(site) && key[:len(site)+1] == site+"/" && f(v) {
			return v, nil
		}
	}
	return Intent{}, ErrNotFound
}
func (s *memStore) Get(_ context.Context, site, id string) (Intent, error) {
	return s.find(site, func(i Intent) bool { return i.ID == id })
}
func (s *memStore) GetByKey(_ context.Context, site, key string) (Intent, error) {
	return s.find(site, func(i Intent) bool { return i.IdempotencyKey == key })
}
func (s *memStore) GetByProviderRequest(_ context.Context, site, p, r string) (Intent, error) {
	return s.find(site, func(i Intent) bool { return i.Provider == p && i.ProviderRequestID == r })
}
func (s *memStore) Transition(_ context.Context, site, id string, o Outcome) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.m[k(site, id)]
	if i.State != StatePending {
		return false, nil
	}
	i.State, i.ProviderReference, i.FailureReason = o.State, o.ProviderReference, o.FailureReason
	s.m[k(site, id)] = i
	return true, nil
}

type fake struct {
	name  string
	out   Outcome
	err   error
	calls int
}

func (f *fake) Name() string { return f.name }
func (f *fake) Request(context.Context, Intent) (Outcome, error) {
	f.calls++
	return f.out, f.err
}

func newSvc(t *testing.T, ps ...Provider) *Service {
	t.Helper()
	r := NewRegistry()
	for _, p := range ps {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{Store: newMem(), Providers: r}
}

func in(provider, key string, amt int64) RequestInput {
	return RequestInput{Provider: provider, Amount: decimal.NewFromInt(amt), Currency: "KES", IdempotencyKey: key}
}

func TestRequest(t *testing.T) {
	ctx := context.Background()
	async := &fake{name: "async", out: Outcome{State: StatePending, ProviderRequestID: "r1"}}
	bad := &fake{name: "bad", err: errors.New("down")}
	s := newSvc(t, async, bad)
	tests := []struct {
		name string
		in   RequestInput
		want State
	}{
		{"cash confirmed", in("cash", "a", 5), StateConfirmed},
		{"async pending", in("async", "b", 5), StatePending},
		{"provider error failed", in("bad", "c", 5), StateFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			it, err := s.Request(ctx, "s", "u", tc.in)
			if err != nil || it.State != tc.want {
				t.Fatalf("got %v %v", it.State, err)
			}
			if tc.want == StateConfirmed && it.ProviderReference != it.ID {
				t.Fatal("cash reference should be intent id")
			}
			if tc.want == StateFailed && it.FailureReason != "down" {
				t.Fatal("reason missing")
			}
		})
	}
}

func TestIdempotency(t *testing.T) {
	ctx := context.Background()
	f := &fake{name: "async", out: Outcome{State: StatePending, ProviderRequestID: "r"}}
	s := newSvc(t, f)
	a, _ := s.Request(ctx, "s", "u", in("async", "k", 5))
	b, err := s.Request(ctx, "s", "u", in("async", "k", 5))
	if err != nil || a.ID != b.ID || f.calls != 1 {
		t.Fatalf("replay: %v %v calls=%d", b.ID, err, f.calls)
	}
	_, err = s.Request(ctx, "s", "u", in("async", "k", 6))
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Type != contract.CodeConflict {
		t.Fatalf("want conflict, got %v", err)
	}
	if _, err = s.Request(ctx, "s", "u", in("async", "", 5)); err == nil {
		t.Fatal("empty key must fail")
	}
	if _, err = s.Request(ctx, "other", "u", in("async", "k", 6)); err != nil {
		t.Fatal("keys are per site")
	}
}

func TestConfirm(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		second  bool
		want    ConfirmResult
		wantErr bool
	}{
		{"duplicate", true, ConfirmDuplicate, false},
		{"conflict", false, ConfirmConflict, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{name: "async", out: Outcome{State: StatePending, ProviderRequestID: "r"}}
			s := newSvc(t, f)
			it, _ := s.Request(ctx, "s", "u", in("async", "k", 5))
			c := Confirmation{Provider: "async", ProviderRequestID: "r", Success: true, ProviderReference: "ref"}
			got, res, err := s.Confirm(ctx, "s", c)
			if err != nil || res != ConfirmApplied || got.State != StateConfirmed || got.ProviderReference != "ref" {
				t.Fatalf("first: %+v %v %v", got, res, err)
			}
			c.Success = tc.second
			if _, res, _ = s.Confirm(ctx, "s", c); res != tc.want {
				t.Fatalf("got %v", res)
			}
			g, _ := s.Get(ctx, "s", it.ID)
			if g.State != StateConfirmed {
				t.Fatal("terminal state must not change")
			}
		})
	}
	s := newSvc(t)
	if _, _, err := s.Confirm(ctx, "s", Confirmation{Provider: "x", ProviderRequestID: "y"}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "s", "nope"); err == nil {
		t.Fatal("want not found")
	}
}

func TestRegistryDuplicate(t *testing.T) {
	if err := NewRegistry().Register(Cash{}); err == nil {
		t.Fatal("duplicate must error")
	}
}

func TestRequestManualRules(t *testing.T) {
	ctx := context.Background()
	reject := &fake{name: "strict", err: contract.NewError(contract.CodeValidationFailed, "rejected by provider")}
	dup := &fake{name: "dup", out: Outcome{State: StateConfirmed, ProviderRequestID: "A:CODE", ProviderReference: "CODE"}}
	s := newSvc(t, reject, dup)

	// A provider validation refusal is returned and no intent is stored.
	_, err := s.Request(ctx, "s", "u", in("strict", "k-reject", 5))
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Message != "rejected by provider" {
		t.Fatalf("validation refusal = %v", err)
	}
	if _, err := s.Store.GetByKey(ctx, "s", "k-reject"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused request stored an intent: %v", err)
	}

	// The same provider request id is refused the second time under a new key.
	if _, err := s.Request(ctx, "s", "u", in("dup", "k-1", 5)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Request(ctx, "s", "u", in("dup", "k-2", 5))
	if !errors.As(err, &ce) || ce.Type != contract.CodeValidationFailed || ce.Message != "this payment reference was already recorded" {
		t.Fatalf("duplicate provider request = %v", err)
	}
	// Replaying the first key is still idempotent, not a duplicate.
	if _, err := s.Request(ctx, "s", "u", in("dup", "k-1", 5)); err != nil {
		t.Fatalf("idempotent replay = %v", err)
	}
	// Another site may record the same reference.
	if _, err := s.Request(ctx, "other", "u", in("dup", "k-3", 5)); err != nil {
		t.Fatalf("same reference on another site = %v", err)
	}
}

func TestPayloadHashBackwardCompatible(t *testing.T) {
	legacy := RequestInput{Provider: "cash", Amount: decimal.NewFromInt(5), Currency: "KES", Contact: "c", SourceDoctype: "Sale"}
	// Stored idempotency keys were hashed before reference/account existed;
	// a request that sets neither must keep hashing the same.
	sum := sha256.Sum256([]byte("cash|5|KES|c|Sale"))
	if got := payloadHash(legacy); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy payload hash changed: %s", got)
	}
	withRef := legacy
	withRef.Reference, withRef.Account = "QK1", "ACC-1"
	withAccountOnly := legacy
	withAccountOnly.Account = "ACC-1"
	if payloadHash(withRef) == payloadHash(legacy) || payloadHash(withRef) == payloadHash(withAccountOnly) {
		t.Fatal("reference and account must be part of the payload hash")
	}
}
