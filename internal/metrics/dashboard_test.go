package metrics

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// testDashboard reads the test-only dashboard in testdata: a tile, a time chart, a dimension chart and a ratio chart.
func testDashboard(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("testdata/till_insights.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func posMetricDefs(t *testing.T) []Metric {
	t.Helper()
	defs, err := ParseDirectory("../../config/pos/metrics")
	if err != nil {
		t.Fatal(err)
	}
	return defs
}

func TestDashboardValidatesAgainstPOSMetrics(t *testing.T) {
	d, err := ParseDashboardYAML([]byte(testDashboard(t)), "till.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDashboards([]Dashboard{d}, posMetricDefs(t), posDoctypes(t)); err != nil {
		t.Fatal(err)
	}
}

// Each case breaks the valid dashboard in one way; the error must name the problem.
func TestDashboardValidationErrors(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"unknown tile key", "name: d\nlabel: D\ndefault_range: today\ntiles:\n  - metric: sales\n    bogus_flag: none\n", `unknown key "bogus_flag"`},
		{"unknown top-level key", "name: d\nlable: D\ndefault_range: today\n", `did you mean "label"`},
		{"unknown chart key", "name: d\nlabel: D\ndefault_range: today\ncharts:\n  - id: c\n    title: C\n    kind: bar\n    mtrics: [sales]\n    by: time\n    grain: day\n", `did you mean "metrics"`},
		{"bad default range", "name: d\nlabel: D\ndefault_range: forever\n", `default_range "forever"`},
		{"name not snake case", "name: Till Insights\nlabel: D\ndefault_range: today\n", "lowercase snake_case"},
		{"tile unknown metric", "name: d\nlabel: D\ndefault_range: today\ntiles:\n  - metric: nope\n", `unknown metric "nope"`},
		{"chart unknown metric", chartYAML("metrics: [nope]\n    by: time\n    grain: day"), `unknown metric "nope"`},
		{"by not a dimension", chartYAML("metrics: [sales]\n    by: color"), `"color" is not a dimension of metric "sales"`},
		{"top with by time", chartYAML("metrics: [sales]\n    by: time\n    grain: day\n    top: 3"), "top needs a dimension"},
		{"time without grain", chartYAML("metrics: [sales]\n    by: time"), `grain "" must be day, week or month`},
		{"grain on a dimension", chartYAML("metrics: [sales]\n    by: register\n    grain: day"), "grain applies to by: time only"},
		{"duplicate chart ids", chartYAML("metrics: [sales]\n    by: time\n    grain: day") + "  - id: c\n    title: C\n    kind: bar\n    metrics: [sales]\n    by: time\n    grain: day\n", `chart "c" is declared more than once`},
		{"more than four metrics", chartYAML("metrics: [sales, refunds, receipts, sales, refunds]\n    by: time\n    grain: day"), "takes 1 to 4 metrics"},
		{"bad chart kind", strings.Replace(chartYAML("metrics: [sales]\n    by: time\n    grain: day"), "kind: bar", "kind: pie", 1), `kind "pie" is not one of`},
	}
	defs, dts := posMetricDefs(t), posDoctypes(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDashboardYAML([]byte(tc.yaml), "t.yaml")
			if err == nil {
				err = ValidateDashboards([]Dashboard{d}, defs, dts)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want it to contain %q", err, tc.want)
			}
		})
	}
}

// chartYAML is a dashboard with one chart whose body (after its id, title and kind) is body.
func chartYAML(body string) string {
	return "name: d\nlabel: D\ndefault_range: today\ncharts:\n  - id: c\n    title: C\n    kind: bar\n    " + body + "\n"
}

func TestDashboardDuplicateNamesRejected(t *testing.T) {
	d, err := ParseDashboardYAML([]byte(testDashboard(t)), "till.yaml")
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateDashboards([]Dashboard{d, d}, posMetricDefs(t), posDoctypes(t))
	if err == nil || !strings.Contains(err.Error(), `dashboard "till_insights" is declared more than once`) {
		t.Fatalf("error = %v; want duplicate dashboard name", err)
	}
}

// Dashboards survive the JSON documents a config snapshot carries and the s-expr snapshot form.
func TestDashboardsSurviveSnapshotForms(t *testing.T) {
	d, err := ParseDashboardYAML([]byte(testDashboard(t)), "till.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := EncodePack(posMetricDefs(t), []Dashboard{d}, posDoctypes(t))
	if err != nil {
		t.Fatal(err)
	}
	var back Dashboard
	if err = json.Unmarshal(raw[0], &back); err != nil || !reflect.DeepEqual(back, d) {
		t.Fatalf("json round trip = %+v, %v; want %+v", back, err, d)
	}
	snap, err := doctype.FromSExpr(doctype.ToSExpr(&doctype.ConfigSnapshot{Dashboards: raw}))
	if err != nil || len(snap.Dashboards) != 1 || string(snap.Dashboards[0]) != string(raw[0]) {
		t.Fatalf("s-expr round trip: %d dashboards, %v", len(snap.Dashboards), err)
	}
}
