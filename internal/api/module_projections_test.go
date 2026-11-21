package api

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/projection"
)

func TestFilterPublishedProjectionRowsKeepsOnlyActorRows(t *testing.T) {
	t.Parallel()
	data := json.RawMessage(`[{"id":"1","owner":"alice","assigned_to":"bob"},{"id":"2","owner":"carol","assigned_to":"alice"},{"id":"3","owner":"carol","assigned_to":"dan"}]`)
	tests := []struct {
		name  string
		scope projection.ActorScope
		want  string
	}{
		{name: "owner only", scope: projection.ActorScope{SubjectID: "alice", OwnerOnly: true, OwnerField: "owner"}, want: `[{"id":"1","owner":"alice","assigned_to":"bob"}]`},
		{name: "assigned field", scope: projection.ActorScope{SubjectID: "alice", AssignedFields: []string{"assigned_to"}}, want: `[{"id":"2","owner":"carol","assigned_to":"alice"}]`},
		{name: "owner or assigned", scope: projection.ActorScope{SubjectID: "alice", OwnerOnly: true, OwnerField: "owner", AssignedFields: []string{"assigned_to"}}, want: `[{"id":"1","owner":"alice","assigned_to":"bob"},{"id":"2","owner":"carol","assigned_to":"alice"}]`},
		{name: "no grant", scope: projection.ActorScope{SubjectID: "eve"}, want: `[]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := filterPublishedProjectionRows(data, tc.scope)
			if err != nil {
				t.Fatal("filter rows:", err)
			}
			var gotRows, wantRows []map[string]any
			if err := json.Unmarshal(got, &gotRows); err != nil {
				t.Fatal("decode filtered rows:", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantRows); err != nil {
				t.Fatal("decode expected rows:", err)
			}
			gotJSON, _ := json.Marshal(gotRows)
			wantJSON, _ := json.Marshal(wantRows)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("rows = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}
