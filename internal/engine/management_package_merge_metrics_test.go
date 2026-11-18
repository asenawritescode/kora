package engine

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func metricDef(name, label string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"name": name, "label": label})
	return raw
}

// A management package's metrics are added to the tenant's and replace same-named ones.
func TestMergeManagementPackageSnapshotMergesMetrics(t *testing.T) {
	current := &doctype.ConfigSnapshot{Metrics: []json.RawMessage{metricDef("sales", "Old"), metricDef("keep", "Keep")}}
	incoming := &doctype.ConfigSnapshot{Metrics: []json.RawMessage{metricDef("sales", "New"), metricDef("refunds", "Refunds")}}
	merged, err := mergeManagementPackageSnapshot(current, incoming)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, raw := range merged.Metrics {
		var m struct{ Name, Label string }
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		got[m.Name] = m.Label
	}
	want := map[string]string{"sales": "New", "keep": "Keep", "refunds": "Refunds"}
	if len(got) != len(want) {
		t.Fatalf("merged metrics = %v; want %v", got, want)
	}
	for name, label := range want {
		if got[name] != label {
			t.Fatalf("metric %s = %q; want %q", name, got[name], label)
		}
	}
}

// The metric_definition resource kind is satisfied by a metric the snapshot carries.
func TestPackageContainsMetricDefinitionResource(t *testing.T) {
	snap := &doctype.ConfigSnapshot{Metrics: []json.RawMessage{metricDef("sales", "Sales")}}
	if !packageContainsResource(snap, "metric_definition", "sales") {
		t.Fatal("snapshot with sales should contain metric_definition sales")
	}
	if packageContainsResource(snap, "metric_definition", "refunds") {
		t.Fatal("snapshot without refunds should not contain metric_definition refunds")
	}
}
