package kernel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// A lifecycle script's nested write skips only the target's role-matrix decision.
// Append-only refusal, entity-owned scope and delete still apply, and a payload
// cannot switch the skip on because the flag is unexported and kernel-set.
func TestLifecycleEffectSkipsOnlyTargetRoleDecision(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{
		{Name: "Effect Source"},
		{Name: "Effect Target"},
		{Name: "Effect Log", AppendOnly: true},
		{Name: "Entity Target", BusinessScopePolicy: contract.ScopeEntityOwned},
	}, []*doctype.Role{{Name: "Clerk"}}, []*doctype.Permission{
		{Doctype: "Effect Source", Role: "Clerk", Read: true, Write: true, Create: true},
		{Doctype: "Effect Target", Role: "Clerk", Read: true},
		{Doctype: "Effect Log", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Entity Target", Role: "Clerk", Read: true},
	})
	actor, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{
		PrincipalID: "clerk", PrincipalType: contract.PrincipalService, Site: "tenant-a", Roles: []string{"Clerk"}, AuthenticatedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		command   string
		doctype   string
		lifecycle bool
		wantErr   bool
	}{
		{"role refuses target update without lifecycle", CommandRecordUpdate, "Effect Target", false, true},
		{"lifecycle update of read-only target allowed", CommandRecordUpdate, "Effect Target", true, false},
		{"lifecycle create of read-only target allowed", CommandRecordCreate, "Effect Target", true, false},
		{"lifecycle delete keeps normal authorization", CommandRecordDelete, "Effect Target", true, true},
		{"lifecycle update of append-only log still refused", CommandRecordUpdate, "Effect Log", true, true},
		{"lifecycle create of append-only log allowed by role", CommandRecordCreate, "Effect Log", true, false},
		{"entity-owned target gets no elevation", CommandRecordUpdate, "Entity Target", true, true},
		{"unknown doctype still not found", CommandRecordUpdate, "Missing", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def, ok := LookupCommand(tc.command)
			if !ok {
				t.Fatalf("command %s not registered", tc.command)
			}
			payload, err := json.Marshal(map[string]string{"doctype": tc.doctype, "name": "x"})
			if err != nil {
				t.Fatal(err)
			}
			op := Operation{Command: tc.command, Payload: payload, Context: OperationContext{
				Site: "tenant-a", TenantID: "tenant-a", Execution: actor, lifecycleEffect: tc.lifecycle,
			}}
			if authErr := authorize(reg, op, def); (authErr != nil) != tc.wantErr {
				t.Fatalf("authorize err=%v wantErr=%v", authErr, tc.wantErr)
			}
		})
	}
}
