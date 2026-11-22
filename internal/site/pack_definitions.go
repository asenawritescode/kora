package site

import (
	"encoding/json"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// LoadPackMetricsAndDashboards reads a pack's metric and dashboard definitions as the JSON documents a config snapshot carries.
func LoadPackMetricsAndDashboards(path string, doctypes []*doctype.DocType) (metricDefs, dashboardDefs []json.RawMessage, err error) {
	return metrics.LoadPack(path, doctypes)
}
