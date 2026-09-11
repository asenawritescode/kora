package tests

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/inventory"
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
	ledger := inventory.NewLedger(inventory.Policy{})
	runtime := org.NewRuntime(sink)
	stockRef := contract.ResourceRef{Namespace: "inventory", Name: "add_stock", Version: 1}
	if err := runtime.RegisterCapability(org.Capability{
		Contract: contract.CapabilityContract{Ref: stockRef, Effects: []string{"stock_movement", "stock_balance_projection"}},
		Handler: func(_ context.Context, intent org.Intent) (json.RawMessage, error) {
			var input struct {
				Item     string `json:"item"`
				Location string `json:"location"`
				Quantity int64  `json:"quantity"`
			}
			if err := json.Unmarshal(intent.Arguments, &input); err != nil {
				return nil, err
			}
			if err := ledger.Append(inventory.Movement{ID: intent.ID, Item: input.Item, Location: input.Location, Quantity: input.Quantity, Kind: inventory.MovementReceive, Actor: intent.Actor.PrincipalID, Reason: "goods receipt"}); err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"item": input.Item, "quantity": input.Quantity})
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Grant(org.Grant{Capability: stockRef.String(), ActorType: contract.PrincipalHuman, ActorID: "operator-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, org.Intent{ID: "movement-1", Site: "acme", Actor: contract.ActorContext{PrincipalID: "operator-1", PrincipalType: contract.PrincipalHuman}, Capability: stockRef, Arguments: json.RawMessage(`{"item":"item-1","location":"warehouse-1","quantity":10}`), CorrelationID: "flow-1"}); err != nil {
		t.Fatal(err)
	}
	if ledger.Balance("item-1", "warehouse-1") != 10 {
		t.Fatal("stock balance was not projected from movement")
	}
	if _, low := ledger.LowStock("item-1", "warehouse-1", 15); !low {
		t.Fatal("low-stock detection did not trigger")
	}
	if len(sink.events) != 1 || sink.events[0].CausationID != "movement-1" {
		t.Fatalf("missing execution provenance: %+v", sink.events)
	}
}
