package site

import (
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// LoadPackReportsAndMetrics reads a pack's reports, metric and dashboard definitions as the JSON documents a config snapshot carries.
func LoadPackReportsAndMetrics(path string, doctypes []*doctype.DocType) (reports, metricDefs, dashboardDefs []json.RawMessage, err error) {
	defs, err := analytics.ParseReportsDirectory(path + "/reports")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parsing reports: %w", err)
	}
	return encodeReportsAndMetrics(defs, path, doctypes)
}

// encodeReportsAndMetrics encodes parsed reports and the pack's metric and dashboard definitions.
func encodeReportsAndMetrics(defs []analytics.ReportDefinition, path string, doctypes []*doctype.DocType) (reports, metricDefs, dashboardDefs []json.RawMessage, err error) {
	reports = make([]json.RawMessage, 0, len(defs))
	for _, report := range defs {
		data, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			return nil, nil, nil, fmt.Errorf("encoding report %s: %w", report.Name, marshalErr)
		}
		reports = append(reports, data)
	}
	metricDefs, dashboardDefs, err = metrics.LoadPack(path, doctypes)
	return reports, metricDefs, dashboardDefs, err
}
