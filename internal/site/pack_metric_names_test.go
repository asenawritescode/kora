package site

import (
	"strings"
	"testing"
)

const salesMetric = "name: sales\nlabel: Sales\ntype: simple\nmodel: Sale\nmeasure:\n  agg: count\n"

// Metric names are claimed across the whole composed package: a root file and a component file cannot share one.
func TestValidatePackRejectsMetricNameCollisionAcrossFiles(t *testing.T) {
	files := []PackFile{
		{Path: "metrics/sales.yaml", ContentType: "metric", Content: salesMetric},
		{Path: "metrics/components/shop/sales.yaml", ContentType: "metric", Content: salesMetric},
	}
	result, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "retail", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range result.Findings {
		if finding.Code == "resource_conflict" && finding.Path == "metrics/components/shop/sales.yaml" && strings.Contains(finding.Message, `metric "sales"`) {
			return
		}
	}
	t.Fatalf("expected a metric resource_conflict finding, got %#v", result.Findings)
}
