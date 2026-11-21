package engine

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func dashboardDef(name, label string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"name": name, "label": label})
	return raw
}

// A management package's dashboards are added to the tenant's and replace same-named ones; omission never deletes.
func TestMergeManagementPackageSnapshotMergesDashboards(t *testing.T) {
	current := &doctype.ConfigSnapshot{Dashboards: []json.RawMessage{dashboardDef("till", "Old"), dashboardDef("keep", "Keep")}}
	incoming := &doctype.ConfigSnapshot{Dashboards: []json.RawMessage{dashboardDef("till", "New"), dashboardDef("stock", "Stock")}}
	merged, err := mergeManagementPackageSnapshot(current, incoming, packageAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, raw := range merged.Dashboards {
		var d struct{ Name, Label string }
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		got[d.Name] = d.Label
	}
	want := map[string]string{"till": "New", "keep": "Keep", "stock": "Stock"}
	if len(got) != len(want) {
		t.Fatalf("merged dashboards = %v; want %v", got, want)
	}
	for name, label := range want {
		if got[name] != label {
			t.Fatalf("dashboard %s = %q; want %q", name, got[name], label)
		}
	}
}
