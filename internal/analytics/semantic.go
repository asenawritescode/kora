package analytics

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
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
		// Module-owned records are never automatically published to the
		// analytics surface. Owners must expose an explicit published query or
		// projection before another module can report on them.
		if dt.ModuleOwnerID != "" {
			continue
		}
		catalog.Models = append(catalog.Models, BuildSemanticModel(dt))
	}
	return catalog
}

// BuildPublishedSemanticModel creates a reporting model solely from an owner's
// declared query output fields. It has no DocType/table source, so analytics
// execution cannot fall through to private record storage.
func BuildPublishedSemanticModel(name, label, owner, projection, query string, scopePolicy contract.ResourceScopeKind, fields []SemanticDimension, measures []SemanticMeasure) (SemanticModel, error) {
	model := SemanticModel{Name: name, Label: label, ProjectionOwner: owner, ProjectionID: projection, PublishedQuery: query, ProjectionScopePolicy: scopePolicy, Dimensions: append([]SemanticDimension(nil), fields...), Measures: append([]SemanticMeasure(nil), measures...)}
	allowed := make(map[string]bool, len(fields)+len(measures))
	for _, dimension := range fields {
		allowed[dimension.Field] = true
		if dimension.Type == "time" && model.TimeDimension == "" {
			model.TimeDimension = dimension.Name
		}
	}
	for _, measure := range measures {
		if measure.Field != "" {
			allowed[measure.Field] = true
		}
	}
	for field := range allowed {
		model.AllowedFields = append(model.AllowedFields, field)
	}
	sort.Strings(model.AllowedFields)
	if err := model.Validate(); err != nil {
		return SemanticModel{}, err
	}
	return model, nil
}

// BuildPublishedProjectionModel creates an analytics model backed only by a
// module projection. Its owner-controlled field allowlist is retained with the
// projection, so historical reports remain readable if an input source goes away.
func BuildPublishedProjectionModel(name, label string, definition contract.ProjectionDefinition, dimensions []SemanticDimension, measures []SemanticMeasure) (SemanticModel, error) {
	if err := definition.Validate(); err != nil {
		return SemanticModel{}, fmt.Errorf("published projection definition is invalid")
	}
	model, err := BuildPublishedSemanticModel(name, label, string(definition.OwnerModuleID), definition.ID, "projection:"+definition.ID, definition.ScopePolicy, dimensions, measures)
	if err != nil {
		return SemanticModel{}, err
	}
	model.AllowedFields = append([]string(nil), definition.AllowedFields...)
	model.ProjectionOwnerField = definition.OwnerField
	sort.Strings(model.AllowedFields)
	if err := model.Validate(); err != nil {
		return SemanticModel{}, err
	}
	return model, nil
}

// AddReportModels adds explicitly configured business models to a catalog.
// This lets a report expose a parent/child concept without making every child
// DocType an independent analytics dataset.
func AddReportModels(catalog *SemanticCatalog, reports []ReportDefinition) error {
	if catalog == nil {
		return fmt.Errorf("analytics catalog is required")
	}
	seen := make(map[string]SemanticModel, len(catalog.Models))
	for _, model := range catalog.Models {
		seen[model.Name] = model
	}
	for _, report := range reports {
		for _, model := range report.Models {
			if existing, ok := seen[model.Name]; ok {
				if !reflect.DeepEqual(existing, model) {
					return fmt.Errorf("report model %q conflicts with an existing model", model.Name)
				}
				continue
			}
			catalog.Models = append(catalog.Models, model)
			seen[model.Name] = model
		}
	}
	return nil
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
		if field.IsLayoutField() || field.Fieldtype == "Table" || field.Fieldname == "" || field.Hidden || field.Fieldtype == "Password" {
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
		case "Select", "Link", "Dynamic Link", "User":
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
	Name                  string                     `json:"name" yaml:"name"`
	Label                 string                     `json:"label" yaml:"label"`
	Description           string                     `json:"description,omitempty" yaml:"description,omitempty"`
	SourceDoctype         string                     `json:"source_doctype,omitempty" yaml:"source_doctype,omitempty"`
	TimeDimension         string                     `json:"time_dimension,omitempty" yaml:"time_dimension,omitempty"`
	Dimensions            []SemanticDimension        `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	Measures              []SemanticMeasure          `json:"measures,omitempty" yaml:"measures,omitempty"`
	Rollups               []RollupDefinition         `json:"rollups,omitempty" yaml:"rollups,omitempty"`
	Relation              *SemanticRelation          `json:"relation,omitempty" yaml:"relation,omitempty"`
	ProjectionOwner       string                     `json:"projection_owner,omitempty" yaml:"projection_owner,omitempty"`
	ProjectionID          string                     `json:"projection_id,omitempty" yaml:"projection_id,omitempty"`
	ProjectionOwnerField  string                     `json:"projection_owner_field,omitempty" yaml:"projection_owner_field,omitempty"`
	PublishedQuery        string                     `json:"published_query,omitempty" yaml:"published_query,omitempty"`
	ProjectionScopePolicy contract.ResourceScopeKind `json:"projection_scope_policy,omitempty" yaml:"projection_scope_policy,omitempty"`
	AllowedFields         []string                   `json:"allowed_fields,omitempty" yaml:"allowed_fields,omitempty"`
}

// SemanticRelation describes a business-level parent/child source. Child
// DocTypes are not exposed as independent models; a report must explicitly
// declare the parent table field through which its rows are interpreted.
type SemanticRelation struct {
	ParentDoctype   string                   `json:"parent_doctype" yaml:"parent_doctype"`
	ChildDoctype    string                   `json:"child_doctype" yaml:"child_doctype"`
	ChildTableField string                   `json:"child_table_field" yaml:"child_table_field"`
	ParentTimeField string                   `json:"parent_time_field" yaml:"parent_time_field"`
	Filters         []SemanticRelationFilter `json:"filters,omitempty" yaml:"filters,omitempty"`
}

type SemanticRelationFilter struct {
	Source   string `json:"source" yaml:"source"` // parent or child
	Field    string `json:"field" yaml:"field"`
	Operator string `json:"operator" yaml:"operator"`
	Value    string `json:"value" yaml:"value"`
}

type SemanticDimension struct {
	Name                   string   `json:"name" yaml:"name"`
	Label                  string   `json:"label" yaml:"label"`
	Field                  string   `json:"field" yaml:"field"`
	Type                   string   `json:"type" yaml:"type"` // category, time, number, boolean, text
	SupportedGranularities []string `json:"supported_granularities,omitempty" yaml:"supported_granularities,omitempty"`
	Source                 string   `json:"source,omitempty" yaml:"source,omitempty"` // parent or child for related models
	LinkDoctype            string   `json:"link_doctype,omitempty" yaml:"link_doctype,omitempty"`
	LinkLabelField         string   `json:"link_label_field,omitempty" yaml:"link_label_field,omitempty"`
}

type SemanticMeasure struct {
	Name        string           `json:"name" yaml:"name"`
	Label       string           `json:"label" yaml:"label"`
	Field       string           `json:"field,omitempty" yaml:"field,omitempty"`
	Aggregation string           `json:"aggregation" yaml:"aggregation"`           // count, sum, avg, min, max
	Format      string           `json:"format,omitempty" yaml:"format,omitempty"` // number, currency, percent
	Filters     []SemanticFilter `json:"filters,omitempty" yaml:"filters,omitempty"`
	Source      string           `json:"source,omitempty" yaml:"source,omitempty"` // parent or child for related models
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
	Models      []SemanticModel `json:"models,omitempty" yaml:"models,omitempty"`
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

// SemanticRequest is a catalog-only analytics query: one or more model queries
// over a time range, with an optional result row limit.
type SemanticRequest struct {
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

const (
	MaxSemanticQueries       = 5
	MaxSemanticResultRows    = 200
	MaxSemanticMeasures      = 20
	MaxSemanticFilters       = 20
	MaxSemanticFilterValues  = 20
	MaxSemanticFilterBytes   = 256
	MaxSemanticDateRangeDays = 3660
)

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
	if m.ProjectionOwner != "" {
		if m.SourceDoctype != "" || m.Relation != nil || m.ProjectionID == "" || m.PublishedQuery == "" || len(m.AllowedFields) == 0 || m.ProjectionScopePolicy != contract.ScopeTenantShared && m.ProjectionScopePolicy != contract.ScopeEntityOwned {
			return fmt.Errorf("published projection model %q requires an owner, projection, query, and field contract without table sources", m.Name)
		}
		allowed := map[string]bool{}
		for _, field := range m.AllowedFields {
			if err := validateIdentifier("published field", field); err != nil {
				return err
			}
			if allowed[field] {
				return fmt.Errorf("duplicate published field %q", field)
			}
			allowed[field] = true
		}
		if m.ProjectionOwnerField != "" && !allowed[m.ProjectionOwnerField] {
			return fmt.Errorf("projection owner field is outside the published field contract")
		}
		for _, dimension := range m.Dimensions {
			if !allowed[dimension.Field] {
				return fmt.Errorf("dimension %q is outside the published query field contract", dimension.Name)
			}
		}
		for _, measure := range m.Measures {
			if measure.Field != "" && !allowed[measure.Field] {
				return fmt.Errorf("measure %q is outside the published query field contract", measure.Name)
			}
		}
	} else if m.SourceDoctype == "" && len(m.Measures) == 0 {
		return fmt.Errorf("source_doctype or measures are required")
	}
	if m.TimeDimension != "" && !hasDimensionField(m.Dimensions, m.TimeDimension) {
		return fmt.Errorf("time_dimension %q is not a declared dimension field", m.TimeDimension)
	}
	if relation := m.Relation; relation != nil {
		if relation.ParentDoctype == "" || relation.ChildDoctype == "" || relation.ChildTableField == "" || relation.ParentTimeField == "" {
			return fmt.Errorf("related model %q requires parent, child, child_table_field, and parent_time_field", m.Name)
		}
		if m.SourceDoctype != relation.ParentDoctype {
			return fmt.Errorf("related model %q source_doctype must match its parent doctype", m.Name)
		}
		if m.TimeDimension == "" {
			return fmt.Errorf("related model %q requires a parent time dimension", m.Name)
		}
		for _, filter := range relation.Filters {
			if (filter.Source != "parent" && filter.Source != "child") || filter.Field == "" || filter.Value == "" {
				return fmt.Errorf("related model %q has an invalid fixed filter", m.Name)
			}
			if filter.Operator != "=" && filter.Operator != "!=" {
				return fmt.Errorf("related model %q has unsupported fixed filter operator %q", m.Name, filter.Operator)
			}
		}
	}

	dimensions := map[string]bool{}
	for _, dimension := range m.Dimensions {
		if err := validateIdentifier("dimension name", dimension.Name); err != nil {
			return err
		}
		if dimension.Field == "" || dimension.Type == "" {
			return fmt.Errorf("dimension %q requires field and type", dimension.Name)
		}
		if m.Relation != nil && dimension.Source != "parent" && dimension.Source != "child" {
			return fmt.Errorf("related model dimension %q must declare parent or child source", dimension.Name)
		}
		if (dimension.LinkDoctype == "") != (dimension.LinkLabelField == "") {
			return fmt.Errorf("dimension %q must declare both link_doctype and link_label_field", dimension.Name)
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
		if m.Relation != nil && measure.Source != "parent" && measure.Source != "child" {
			return fmt.Errorf("related model measure %q must declare parent or child source", measure.Name)
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

// Validate checks the request against catalog and the query, measure, filter,
// range and row limits, returning the first violation.
func (q *SemanticRequest) Validate(catalog *SemanticCatalog) error {
	if catalog == nil {
		return fmt.Errorf("analytics catalog is required")
	}
	if len(q.Queries) == 0 || len(q.Queries) > MaxSemanticQueries {
		return fmt.Errorf("query count must be between 1 and %d", MaxSemanticQueries)
	}
	if q.Limit < 0 || q.Limit > MaxSemanticResultRows {
		return fmt.Errorf("limit must be between 0 and %d", MaxSemanticResultRows)
	}
	if q.Time.From == "" || q.Time.To == "" {
		return fmt.Errorf("time.from and time.to are required")
	}
	from, fromErr := time.Parse("2006-01-02", q.Time.From)
	to, toErr := time.Parse("2006-01-02", q.Time.To)
	if fromErr != nil || toErr != nil {
		return fmt.Errorf("time.from and time.to must be valid ISO dates")
	}
	if from.After(to) {
		return fmt.Errorf("time.from must not be after time.to")
	}
	if int(to.Sub(from).Hours()/24)+1 > MaxSemanticDateRangeDays {
		return fmt.Errorf("date range must not exceed %d days", MaxSemanticDateRangeDays)
	}
	if q.Time.Granularity != "" && !validGranularity(q.Time.Granularity) {
		return fmt.Errorf("unsupported query granularity %q", q.Time.Granularity)
	}
	totalMeasures := 0
	totalFilters := 0
	for i, query := range q.Queries {
		if len(query.Measures) == 0 || len(query.Measures) > MaxSemanticMeasures {
			return fmt.Errorf("queries[%d]: measure count must be between 1 and %d", i, MaxSemanticMeasures)
		}
		totalMeasures += len(query.Measures)
		if totalMeasures > MaxSemanticMeasures {
			return fmt.Errorf("request measure count exceeds %d", MaxSemanticMeasures)
		}
		if len(query.Dimensions) > 1 {
			return fmt.Errorf("queries[%d]: at most one dimension is supported", i)
		}
		if len(query.Filters) > MaxSemanticFilters {
			return fmt.Errorf("queries[%d]: filter count exceeds %d", i, MaxSemanticFilters)
		}
		totalFilters += len(query.Filters)
		if totalFilters > MaxSemanticFilters {
			return fmt.Errorf("request filter count exceeds %d", MaxSemanticFilters)
		}
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
			if len(filter.Values) == 0 || len(filter.Values) > MaxSemanticFilterValues {
				return fmt.Errorf("queries[%d]: filter %q must have between 1 and %d values", i, filter.Field, MaxSemanticFilterValues)
			}
			for _, value := range filter.Values {
				if len(value) > MaxSemanticFilterBytes {
					return fmt.Errorf("queries[%d]: filter %q value exceeds %d bytes", i, filter.Field, MaxSemanticFilterBytes)
				}
			}
			operator := strings.ToLower(strings.TrimSpace(filter.Operator))
			allowedOperators := map[string]bool{"=": true, "==": true, "!=": true, "<>": true, "in": true, "not in": true, "like": true, "not like": true}
			if !allowedOperators[operator] {
				return fmt.Errorf("queries[%d]: unsupported filter operator %q", i, filter.Operator)
			}
			if (operator == "=" || operator == "==" || operator == "!=" || operator == "<>" || operator == "like" || operator == "not like") && len(filter.Values) != 1 {
				return fmt.Errorf("queries[%d]: filter operator %q requires exactly one value", i, filter.Operator)
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

func validSemanticFilterOperator(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "=", "==", "!=", "<>", "in", "not in", "like", "not like":
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
