package analytics

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseReportYAML parses one report definition from a configuration file. The
// report remains declarative: execution is still governed by the catalog.
func ParseReportYAML(data []byte, source string) (*ReportDefinition, error) {
	var report ReportDefinition
	if err := yaml.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parsing report %s: %w", source, err)
	}
	if err := report.Validate(nil); err != nil {
		return nil, fmt.Errorf("report %s: %w", source, err)
	}
	return &report, nil
}

func (r *ReportDefinition) Validate(catalog *SemanticCatalog) error {
	if err := validateIdentifier("report name", r.Name); err != nil {
		return err
	}
	if strings.TrimSpace(r.Label) == "" {
		return fmt.Errorf("report %q requires a label", r.Name)
	}
	if len(r.Queries) == 0 {
		return fmt.Errorf("report %q requires at least one query", r.Name)
	}
	seenQueries := map[string]bool{}
	for index, query := range r.Queries {
		if err := validateIdentifier(fmt.Sprintf("query[%d] id", index), query.ID); err != nil {
			return err
		}
		if seenQueries[query.ID] {
			return fmt.Errorf("duplicate query id %q", query.ID)
		}
		seenQueries[query.ID] = true
		if query.Model == "" || len(query.Measures) == 0 {
			return fmt.Errorf("query %q requires a model and at least one measure", query.ID)
		}
		if catalog != nil {
			candidate := AnalyticsQueryRequest{Queries: []ModelQuery{{Model: query.Model, Measures: query.Measures, Dimensions: query.Dimensions, Filters: query.Filters}}}
			if err := candidate.Validate(catalog); err != nil {
				return fmt.Errorf("query %q: %w", query.ID, err)
			}
		}
	}
	seenDerived := map[string]bool{}
	for _, derived := range r.Derived {
		if err := validateIdentifier("derived metric name", derived.Name); err != nil {
			return err
		}
		if strings.TrimSpace(derived.Label) == "" || strings.TrimSpace(derived.Formula) == "" {
			return fmt.Errorf("derived metric %q requires label and formula", derived.Name)
		}
		if seenDerived[derived.Name] {
			return fmt.Errorf("duplicate derived metric %q", derived.Name)
		}
		seenDerived[derived.Name] = true
	}
	for _, visual := range r.Visuals {
		if err := validateIdentifier("visual id", visual.ID); err != nil {
			return err
		}
		if visual.Type == "" || visual.Source == "" {
			return fmt.Errorf("visual %q requires type and source", visual.ID)
		}
		if !seenQueries[visual.Source] {
			return fmt.Errorf("visual %q references unknown query %q", visual.ID, visual.Source)
		}
	}
	return nil
}
