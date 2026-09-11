package tests

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/org"
)

type eventSink struct{ events []contract.EventEnvelope }

func (s *eventSink) Publish(_ context.Context, event contract.EventEnvelope) error {
	s.events = append(s.events, event)
	return nil
}

func TestOrganizationalRuntimeAcceptance(t *testing.T) {
	ctx := context.Background()
	sink := &eventSink{}
	runtime := org.NewRuntime(sink)
	capabilityRef := contract.ResourceRef{Namespace: "package", Name: "create_record", Version: 1}
	if err := runtime.RegisterCapability(org.Capability{
		Contract: contract.CapabilityContract{Ref: capabilityRef, Effects: []string{"record_created"}},
		Handler: func(_ context.Context, intent org.Intent) (json.RawMessage, error) {
			var input map[string]any
			if err := json.Unmarshal(intent.Arguments, &input); err != nil {
				return nil, err
			}
			return json.Marshal(input)
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Grant(org.Grant{Capability: capabilityRef.String(), ActorType: contract.PrincipalHuman, ActorID: "operator-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, org.Intent{ID: "operation-1", Site: "acme", Actor: contract.ActorContext{PrincipalID: "operator-1", PrincipalType: contract.PrincipalHuman}, Capability: capabilityRef, Arguments: json.RawMessage(`{"record_type":"example","quantity":10}`), CorrelationID: "flow-1"}); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 || sink.events[0].CausationID != "operation-1" {
		t.Fatalf("missing execution provenance: %+v", sink.events)
	}
}
