package metrics

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestPOSMetricPackLoadsAndValidates(t *testing.T) {
	defs, err := ParseDirectory("../../config/pos/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 4 {
		t.Fatalf("got %d metrics; want 4", len(defs))
	}
	raw, _, err := LoadPack("../../config/pos", posDoctypes(t))
	if err != nil {
		t.Fatal(err)
	}
	var back Metric
	if err := json.Unmarshal(raw[0], &back); err != nil || len(raw) != len(defs) || back.Name != "average_sale" || back.Numerator != "sales" {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}

func TestMetricsSurviveSExprSnapshot(t *testing.T) {
	raw, _, err := LoadPack("../../config/pos", posDoctypes(t))
	if err != nil {
		t.Fatal(err)
	}
	back, err := doctype.FromSExpr(doctype.ToSExpr(&doctype.ConfigSnapshot{Metrics: raw}))
	if err != nil || len(back.Metrics) != len(raw) || string(back.Metrics[0]) != string(raw[0]) {
		t.Fatalf("s-expr round trip: %d metrics, %v", len(back.Metrics), err)
	}
}

func TestParseUnknownKeysWithLineAndSuggestion(t *testing.T) {
	_, err := ParseYAML([]byte("name: sales\nlabel: Sales\ntype: simple\nmodel: Sale\nmesure:\n  agg: count\n"), "t.yaml")
	if err == nil || !strings.Contains(err.Error(), "line 5") || !strings.Contains(err.Error(), `did you mean "measure"`) {
		t.Fatalf("error = %v; want line 5 and a did-you-mean hint", err)
	}
	_, err = ParseYAML([]byte("name: sales\nlabel: Sales\ntype: simple\nmodel: Sale\nmeasure:\n  agg: count\n  feild: total\n"), "t.yaml")
	if err == nil || !strings.Contains(err.Error(), `did you mean "field"`) {
		t.Fatalf("nested error = %v; want did-you-mean for field", err)
	}
}
