package analytics

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/asenawritescode/kora/doctype"
)

// SemanticCatalog is the governed vocabulary exposed to report builders and
// other analytics consumers. It deliberately describes business concepts,
// rather than exposing arbitrary SQL or storage details.
type SemanticCatalog struct {
	Models  []SemanticModel    `json:"models" yaml:"models"`
	Reports []ReportDefinition `json:"reports,omitempty" yaml:"reports,omitempty"`
}

// BuildSemanticCatalog exposes the safe, automatically discoverable portion of
// the registry to analytics consumers.
func BuildSemanticCatalog(docTypes []*doctype.DocType) *SemanticCatalog {
	catalog := &SemanticCatalog{Models: make([]SemanticModel, 0, len(docTypes))}
	for _, dt := range docTypes {
		if dt == nil || dt.IsChildTable || dt.IsSingle {
			continue
		}
		catalog.Models = append(catalog.Models, BuildSemanticModel(dt))
	}
	return catalog
}

// BuildSemanticModel creates a conservative catalog model from DocType
// metadata. Explicit semantic configuration can later enrich or replace this
// projection without changing the query contract.
func BuildSemanticModel(dt *doctype.DocType) SemanticModel {
	model := SemanticModel{
		Name:          metricName(dt.Name),
		Label:         dt.Name,
		SourceDoctype: dt.Name,
	}
	for _, field := range dt.Fields {
		if field.IsLayoutField() || field.Fieldtype == "Table" || field.Fieldname == "" {
			continue
		}
		label := field.Label
		if label == "" {
			label = field.Fieldname
		}
		switch field.Fieldtype {
		case "Date", "Datetime":
			model.Dimensions = append(model.Dimensions, SemanticDimension{
				Name: field.Fieldname, Label: label, Field: field.Fieldname,
				Type: "time", SupportedGranularities: []string{"day", "week", "month", "quarter", "year"},
			})
			if model.TimeDimension == "" || field.Fieldname == dt.SortField {
				model.TimeDimension = field.Fieldname
			}
		case "Select", "Link", "Dynamic Link":
			model.Dimensions = append(model.Dimensions, SemanticDimension{
				Name: field.Fieldname, Label: label, Field: field.Fieldname, Type: "category",
			})
		case "Check":
			model.Dimensions = append(model.Dimensions, SemanticDimension{
				Name: field.Fieldname, Label: label, Field: field.Fieldname, Type: "boolean",
			})
		}
		if field.IsNumeric() {
			format := "number"
			if field.Fieldtype == "Currency" {
				format = "currency"
			} else if field.Fieldtype == "Percent" {
				format = "percent"
			}
			model.Measures = append(model.Measures, SemanticMeasure{
				Name: field.Fieldname + "_sum", Label: "Total " + label,
				Field: field.Fieldname, Aggregation: "sum", Format: format,
			})
		}
	}
	if model.TimeDimension == "" {
		model.TimeDimension = "creation"
		model.Dimensions = append(model.Dimensions, SemanticDimension{
			Name: "creation", Label: "Created", Field: "creation", Type: "time",
			SupportedGranularities: []string{"day", "week", "month", "quarter", "year"},
		})
	}
	model.Measures = append([]SemanticMeasure{{
		Name: "count", Label: "Count", Aggregation: "count", Format: "number",
	}}, model.Measures...)
	return model
}

// GenerateDefaultReports creates a small, governed overview report for every
// model that does not provide an explicit report definition. It uses only
// fields already exposed by the semantic catalog, so new config packs get a
// useful analytics entry point without inventing business meaning or SQL.
func GenerateDefaultReports(catalog *SemanticCatalog) []ReportDefinition {
	if catalog == nil {
		return []ReportDefinition{}
	}
	reports := make([]ReportDefinition, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		query := ReportQuery{ID: "overview", Model: model.Name, Measures: []string{"count"}}
		series := []VisualSeries{{Field: "count", Label: "Records", Type: "bar", Format: "number"}}
		if model.TimeDimension != "" {
			query.Dimensions = append(query.Dimensions, model.TimeDimension)
		}
		for _, measure := range model.Measures {
			if measure.Name == "count" {
				continue
			}
			query.Measures = append(query.Measures, measure.Name)
			series = append(series, VisualSeries{Field: measure.Name, Label: measure.Label, Type: "line", Format: measure.Format})
			break
		}
		reports = append(reports, ReportDefinition{
			Name:    model.Name + "_overview",
			Label:   model.Label + " overview",
			Route:   "/reports/" + model.Name + "-overview",
			Queries: []ReportQuery{query},
			Visuals: []ReportVisual{{ID: "overview_trend", Type: "combo", Source: "overview", XField: model.TimeDimension, Series: series}},
		})
	}
	return reports
}

// SemanticModel is an analytics-friendly projection of one or more sources.
type SemanticModel struct {
	Name          string              `json:"name" yaml:"name"`
	Label         string              `json:"label" yaml:"label"`
	Description   string              `json:"description,omitempty" yaml:"description,omitempty"`
	SourceDoctype string              `json:"source_doctype,omitempty" yaml:"source_doctype,omitempty"`
	TimeDimension string              `json:"time_dimension,omitempty" yaml:"time_dimension,omitempty"`
	Dimensions    []SemanticDimension `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	Measures      []SemanticMeasure   `json:"measures,omitempty" yaml:"measures,omitempty"`
	Rollups       []RollupDefinition  `json:"rollups,omitempty" yaml:"rollups,omitempty"`
}

type SemanticDimension struct {
	Name                   string   `json:"name" yaml:"name"`
	Label                  string   `json:"label" yaml:"label"`
	Field                  string   `json:"field" yaml:"field"`
	Type                   string   `json:"type" yaml:"type"` // category, time, number, boolean, text
	SupportedGranularities []string `json:"supported_granularities,omitempty" yaml:"supported_granularities,omitempty"`
}

type SemanticMeasure struct {
	Name        string           `json:"name" yaml:"name"`
	Label       string           `json:"label" yaml:"label"`
	Field       string           `json:"field,omitempty" yaml:"field,omitempty"`
	Aggregation string           `json:"aggregation" yaml:"aggregation"`           // count, sum, avg, min, max
	Format      string           `json:"format,omitempty" yaml:"format,omitempty"` // number, currency, percent
	Filters     []SemanticFilter `json:"filters,omitempty" yaml:"filters,omitempty"`
}

type SemanticFilter struct {
	Field    string   `json:"field" yaml:"field"`
	Operator string   `json:"operator" yaml:"operator"`
	Values   []string `json:"values" yaml:"values"`
}

type RollupDefinition struct {
	Name            string   `json:"name" yaml:"name"`
	Measures        []string `json:"measures" yaml:"measures"`
	Dimensions      []string `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	TimeDimension   string   `json:"time_dimension,omitempty" yaml:"time_dimension,omitempty"`
	Granularity     string   `json:"granularity" yaml:"granularity"` // day, week, month, quarter, year
	RefreshInterval string   `json:"refresh_interval,omitempty" yaml:"refresh_interval,omitempty"`
	PartitionBy     string   `json:"partition_by,omitempty" yaml:"partition_by,omitempty"`
}

type ReportDefinition struct {
	Name        string          `json:"name" yaml:"name"`
	Label       string          `json:"label" yaml:"label"`
	Route       string          `json:"route,omitempty" yaml:"route,omitempty"`
	Permissions []string        `json:"permissions,omitempty" yaml:"permissions,omitempty"`
	Queries     []ReportQuery   `json:"queries" yaml:"queries"`
	Derived     []DerivedMetric `json:"derived,omitempty" yaml:"derived,omitempty"`
	Visuals     []ReportVisual  `json:"visuals,omitempty" yaml:"visuals,omitempty"`
}

type ReportQuery struct {
	ID         string           `json:"id" yaml:"id"`
	Model      string           `json:"model" yaml:"model"`
	Measures   []string         `json:"measures" yaml:"measures"`
	Dimensions []string         `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	Filters    []SemanticFilter `json:"filters,omitempty" yaml:"filters,omitempty"`
}

type DerivedMetric struct {
	Name    string `json:"name" yaml:"name"`
	Label   string `json:"label" yaml:"label"`
	Formula string `json:"formula" yaml:"formula"`
	Format  string `json:"format,omitempty" yaml:"format,omitempty"`
}

type ReportVisual struct {
	ID     string         `json:"id" yaml:"id"`
	Type   string         `json:"type" yaml:"type"`
	Source string         `json:"source" yaml:"source"`
	XField string         `json:"x_field,omitempty" yaml:"x_field,omitempty"`
	Series []VisualSeries `json:"series,omitempty" yaml:"series,omitempty"`
}

type VisualSeries struct {
	Field  string `json:"field" yaml:"field"`
	Label  string `json:"label" yaml:"label"`
	Type   string `json:"type,omitempty" yaml:"type,omitempty"`
	Format string `json:"format,omitempty" yaml:"format,omitempty"`
}

type AnalyticsQueryRequest struct {
	Queries []ModelQuery `json:"queries"`
	Time    QueryTime    `json:"time"`
	Limit   int          `json:"limit,omitempty"`
}

type ModelQuery struct {
	Model      string           `json:"model"`
	Measures   []string         `json:"measures"`
	Dimensions []string         `json:"dimensions,omitempty"`
	Filters    []SemanticFilter `json:"filters,omitempty"`
}

type QueryTime struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Granularity string `json:"granularity"`
}

var semanticIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func (c *SemanticCatalog) Validate() error {
	if c == nil {
		return fmt.Errorf("analytics catalog is required")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		model := &c.Models[i]
		if err := model.Validate(); err != nil {
			return fmt.Errorf("model %q: %w", model.Name, err)
		}
		if seen[model.Name] {
			return fmt.Errorf("duplicate model %q", model.Name)
		}
		seen[model.Name] = true
	}
	for i := range c.Reports {
		if err := c.Reports[i].Validate(c); err != nil {
			return fmt.Errorf("report %q: %w", c.Reports[i].Name, err)
		}
	}
	return nil
}

func (m *SemanticModel) Validate() error {
	if err := validateIdentifier("name", m.Name); err != nil {
		return err
	}
	if m.SourceDoctype == "" && len(m.Measures) == 0 {
		return fmt.Errorf("source_doctype or measures are required")
	}
	if m.TimeDimension != "" && !hasDimensionField(m.Dimensions, m.TimeDimension) {
		return fmt.Errorf("time_dimension %q is not a declared dimension field", m.TimeDimension)
	}

	dimensions := map[string]bool{}
	for _, dimension := range m.Dimensions {
		if err := validateIdentifier("dimension name", dimension.Name); err != nil {
			return err
		}
		if dimension.Field == "" || dimension.Type == "" {
			return fmt.Errorf("dimension %q requires field and type", dimension.Name)
		}
		if dimensions[dimension.Name] {
			return fmt.Errorf("duplicate dimension %q", dimension.Name)
		}
		dimensions[dimension.Name] = true
	}

	measures := map[string]bool{}
	for _, measure := range m.Measures {
		if err := validateIdentifier("measure name", measure.Name); err != nil {
			return err
		}
		if !validAggregation(measure.Aggregation) {
			return fmt.Errorf("measure %q has unsupported aggregation %q", measure.Name, measure.Aggregation)
		}
		if measure.Aggregation != "count" && measure.Field == "" {
			return fmt.Errorf("measure %q requires a field", measure.Name)
		}
		if measures[measure.Name] {
			return fmt.Errorf("duplicate measure %q", measure.Name)
		}
		measures[measure.Name] = true
	}

	for _, rollup := range m.Rollups {
		if err := rollup.Validate(measures, dimensions, m.TimeDimension); err != nil {
			return fmt.Errorf("rollup %q: %w", rollup.Name, err)
		}
	}
	return nil
}

func (r *RollupDefinition) Validate(measures, dimensions map[string]bool, modelTimeDimension string) error {
	if err := validateIdentifier("name", r.Name); err != nil {
		return err
	}
	if !validGranularity(r.Granularity) {
		return fmt.Errorf("unsupported granularity %q", r.Granularity)
	}
	if len(r.Measures) == 0 {
		return fmt.Errorf("at least one measure is required")
	}
	for _, measure := range r.Measures {
		if !measures[measure] {
			return fmt.Errorf("unknown measure %q", measure)
		}
	}
	for _, dimension := range r.Dimensions {
		if !dimensions[dimension] {
			return fmt.Errorf("unknown dimension %q", dimension)
		}
	}
	if r.TimeDimension != "" && !dimensions[r.TimeDimension] {
		return fmt.Errorf("unknown time dimension %q", r.TimeDimension)
	}
	if modelTimeDimension != "" && r.TimeDimension != "" && r.TimeDimension != modelTimeDimension {
		return fmt.Errorf("time dimension must be %q", modelTimeDimension)
	}
	return nil
}

func (q *AnalyticsQueryRequest) Validate(catalog *SemanticCatalog) error {
	if catalog == nil {
		return fmt.Errorf("analytics catalog is required")
	}
	if len(q.Queries) == 0 {
		return fmt.Errorf("at least one query is required")
	}
	if q.Limit < 0 || q.Limit > 10000 {
		return fmt.Errorf("limit must be between 0 and 10000")
	}
	if q.Time.From == "" || q.Time.To == "" {
		return fmt.Errorf("time.from and time.to are required")
	}
	if q.Time.From > q.Time.To {
		return fmt.Errorf("time.from must not be after time.to")
	}
	if q.Time.Granularity != "" && !validGranularity(q.Time.Granularity) {
		return fmt.Errorf("unsupported query granularity %q", q.Time.Granularity)
	}
	for i, query := range q.Queries {
		model := catalog.model(query.Model)
		if model == nil {
			return fmt.Errorf("queries[%d]: unknown model %q", i, query.Model)
		}
		if err := model.Validate(); err != nil {
			return fmt.Errorf("queries[%d]: model %q is invalid: %w", i, query.Model, err)
		}
		measures := map[string]bool{}
		for _, measure := range model.Measures {
			measures[measure.Name] = true
		}
		for _, name := range query.Measures {
			if !measures[name] {
				return fmt.Errorf("queries[%d]: unknown measure %q", i, name)
			}
		}
		dimensions := map[string]bool{}
		for _, dimension := range model.Dimensions {
			dimensions[dimension.Name] = true
		}
		for _, name := range query.Dimensions {
			if !dimensions[name] {
				return fmt.Errorf("queries[%d]: unknown dimension %q", i, name)
			}
		}
		for _, filter := range query.Filters {
			if !dimensions[filter.Field] {
				return fmt.Errorf("queries[%d]: unknown filter field %q", i, filter.Field)
			}
			if len(filter.Values) == 0 {
				return fmt.Errorf("queries[%d]: filter %q requires at least one value", i, filter.Field)
			}
			operator := strings.ToLower(strings.TrimSpace(filter.Operator))
			allowedOperators := map[string]bool{"=": true, "==": true, "!=": true, "<>": true, "in": true, "not in": true, "like": true, "not like": true}
			if !allowedOperators[operator] {
				return fmt.Errorf("queries[%d]: unsupported filter operator %q", i, filter.Operator)
			}
		}
	}
	return nil
}

func (c *SemanticCatalog) model(name string) *SemanticModel {
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i]
		}
	}
	return nil
}

func validateIdentifier(label, value string) error {
	if !semanticIdentifier.MatchString(value) {
		return fmt.Errorf("%s must use lowercase letters, numbers, and underscores", label)
	}
	return nil
}

func validAggregation(value string) bool {
	switch strings.ToLower(value) {
	case "count", "sum", "avg", "min", "max":
		return true
	default:
		return false
	}
}

func validGranularity(value string) bool {
	switch value {
	case "day", "week", "month", "quarter", "year":
		return true
	default:
		return false
	}
}

func hasDimensionField(dimensions []SemanticDimension, field string) bool {
	for _, dimension := range dimensions {
		if dimension.Name == field || dimension.Field == field {
			return true
		}
	}
	return false
}
