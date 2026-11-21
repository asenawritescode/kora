package metrics

import (
	"slices"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

const (
	erpsellPackDir = "../../config/_parked/erpsell"
	posPackDir     = "../../config/pos"
)

func erpsellDoctypes(t *testing.T) []*doctype.DocType {
	t.Helper()
	dts, err := doctype.ParseConfigTree(erpsellPackDir)
	if err != nil {
		t.Fatal(err)
	}
	return dts
}

// The selling pack ships its metrics, the Shop insights and Stock insights dashboards; all load and validate together.
func TestErpsellPackMetricsAndDashboardsValidate(t *testing.T) {
	dts := erpsellDoctypes(t)
	raw, dashRaw, err := LoadPack(erpsellPackDir, dts)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 10 || len(dashRaw) != 2 {
		t.Fatalf("selling pack carries %d metrics and %d dashboards; want 10 and 2", len(raw), len(dashRaw))
	}
	dashboards, err := ParseDashboardDirectory(erpsellPackDir + "/dashboards")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dashboards {
		names = append(names, d.Name)
	}
	if !slices.Equal(names, []string{"shop_insights", "stock_insights"}) {
		t.Fatalf("dashboards = %q; want shop_insights and stock_insights", names)
	}
}

// The POS pack's insights dashboard names only its own metrics and dimensions.
func TestPOSInsightsDashboardValidates(t *testing.T) {
	dashboards, err := ParseDashboardDirectory(posPackDir + "/dashboards")
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboards) != 1 || dashboards[0].Name != "pos_insights" {
		t.Fatalf("dashboards = %+v; want pos_insights", dashboards)
	}
	if err := ValidateDashboards(dashboards, posMetricDefs(t), posDoctypes(t)); err != nil {
		t.Fatal(err)
	}
}
