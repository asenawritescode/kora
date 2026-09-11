package org

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

type publisher struct{ events []contract.EventEnvelope }

func (p *publisher) Publish(_ context.Context, e contract.EventEnvelope) error {
	p.events = append(p.events, e)
	return nil
}

func TestRuntimeUsesExplicitCapabilityGrantAndPublishesEvent(t *testing.T) {
	p := &publisher{}
	r := NewRuntime(p)
	ref := contract.ResourceRef{Namespace: "tenant-a", Name: "stock.add", Version: 1}
	if err := r.RegisterCapability(Capability{Contract: contract.CapabilityContract{Ref: ref}, Handler: func(context.Context, Intent) (json.RawMessage, error) { return json.RawMessage(`{"ok":true}`), nil }}); err != nil {
		t.Fatal(err)
	}
	intent := Intent{ID: "op-1", Site: "acme", Actor: contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman}, Capability: ref}
	if _, err := r.Execute(context.Background(), intent); !errors.Is(err, contract.NewError(contract.CodePermissionDenied, "x")) { // compare code below
		if e, ok := err.(*contract.Error); !ok || e.Type != contract.CodePermissionDenied {
			t.Fatalf("want permission denial, got %v", err)
		}
	}
	if err := r.Grant(Grant{Capability: ref.String(), ActorID: "user-1", ActorType: contract.PrincipalHuman}); err != nil {
		t.Fatal(err)
	}
	result, err := r.Execute(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.CausationID != intent.ID || len(p.events) != 1 {
		t.Fatalf("event provenance not recorded: %+v", result.Event)
	}
}

func TestRevisionStorePreviewActivateAndRollback(t *testing.T) {
	s := NewRevisionStore()
	if _, err := s.Create(Revision{ID: "v1", Author: "user-1", Reason: "initial", Changes: []Change{{Resource: "Item"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Revision{ID: "v2", ParentID: "v1", Author: "user-1", Reason: "add supplier", Changes: []Change{{Resource: "Supplier"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Preview("v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Activate("v2"); err != nil {
		t.Fatal(err)
	}
	active, err := s.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != "v1" || active.State != RevisionActive {
		t.Fatalf("rollback active = %+v", active)
	}
}
