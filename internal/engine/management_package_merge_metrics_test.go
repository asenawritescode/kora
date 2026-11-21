package engine

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/site"
)

func metricDef(name, label string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"name": name, "label": label})
	return raw
}

// A management package's metrics are added to the tenant's and replace same-named ones.
func TestMergeManagementPackageSnapshotMergesMetrics(t *testing.T) {
	current := &doctype.ConfigSnapshot{Metrics: []json.RawMessage{metricDef("sales", "Old"), metricDef("keep", "Keep")}}
	incoming := &doctype.ConfigSnapshot{Metrics: []json.RawMessage{metricDef("sales", "New"), metricDef("refunds", "Refunds")}}
	merged, err := mergeManagementPackageSnapshot(current, incoming, packageAuthority{})
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

// A management merge carries the POS metric definitions a directory install parsed, unchanged.
func TestMergeManagementPackageSnapshotCarriesPOSMetricsUnchanged(t *testing.T) {
	var dts []*doctype.DocType
	for _, pack := range []string{"inventory", "pos"} {
		parsed, err := doctype.ParseConfigTree(filepath.Join("..", "..", "config", pack))
		if err != nil {
			t.Fatal(err)
		}
		dts = append(dts, parsed...)
	}
	_, incoming, err := site.LoadPackMetricsAndDashboards(filepath.Join("..", "..", "config", "pos"), dts)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeManagementPackageSnapshot(&doctype.ConfigSnapshot{}, &doctype.ConfigSnapshot{Metrics: incoming}, packageAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decodeNamedMetrics(t, merged.Metrics), decodeNamedMetrics(t, incoming); !reflect.DeepEqual(got, want) || len(got) == 0 {
		t.Fatalf("merged metrics = %+v; want %+v", got, want)
	}
}

func decodeNamedMetrics(t *testing.T, raw []json.RawMessage) []metrics.Metric {
	t.Helper()
	out := make([]metrics.Metric, 0, len(raw))
	for _, r := range raw {
		var m metrics.Metric
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
