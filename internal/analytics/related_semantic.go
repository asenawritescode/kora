package analytics

import (
	"fmt"

	"github.com/asenawritescode/kora/internal/doctype"
)

// ValidateReportModels verifies configured relationships and fields against
// the live DocType registry before those models are exposed or queried.
func ValidateReportModels(registry *doctype.Registry, reports []ReportDefinition) error {
	if registry == nil {
		return fmt.Errorf("doctype registry is required to validate report models")
	}
	for _, report := range reports {
		for _, model := range report.Models {
			if err := model.Validate(); err != nil {
				return fmt.Errorf("report %q model %q: %w", report.Name, model.Name, err)
			}
			relation := model.Relation
			if relation == nil {
				continue
			}
			parent := registry.Get(relation.ParentDoctype)
			child := registry.Get(relation.ChildDoctype)
			if parent == nil || parent.IsChildTable || parent.IsSingle {
				return fmt.Errorf("report %q model %q has invalid parent doctype %q", report.Name, model.Name, relation.ParentDoctype)
			}
			if child == nil || !child.IsChildTable {
				return fmt.Errorf("report %q model %q requires child doctype %q", report.Name, model.Name, relation.ChildDoctype)
			}
			tableField := parent.GetField(relation.ChildTableField)
			if tableField == nil || tableField.Fieldtype != "Table" || tableField.Options != child.Name {
				return fmt.Errorf("report %q model %q parent field %q does not reference child doctype %q", report.Name, model.Name, relation.ChildTableField, child.Name)
			}
			timeField := parent.GetField(relation.ParentTimeField)
			if timeField == nil || (timeField.Fieldtype != "Date" && timeField.Fieldtype != "Datetime") {
				return fmt.Errorf("report %q model %q parent time field %q must be Date or Datetime", report.Name, model.Name, relation.ParentTimeField)
			}
			for _, dimension := range model.Dimensions {
				source := parent
				if dimension.Source == "child" {
					source = child
				}
				field := source.GetField(dimension.Field)
				if field == nil {
					return fmt.Errorf("report %q model %q dimension %q references unknown %s field %q", report.Name, model.Name, dimension.Name, dimension.Source, dimension.Field)
				}
				if dimension.LinkDoctype != "" {
					linked := registry.Get(dimension.LinkDoctype)
					if (field.Fieldtype != "Link" && field.Fieldtype != "Dynamic Link") || field.Options != dimension.LinkDoctype || linked == nil {
						return fmt.Errorf("report %q model %q dimension %q link metadata does not match its field", report.Name, model.Name, dimension.Name)
					}
					if linked.GetField(dimension.LinkLabelField) == nil {
						return fmt.Errorf("report %q model %q dimension %q references unknown label field %q", report.Name, model.Name, dimension.Name, dimension.LinkLabelField)
					}
				}
			}
			for _, measure := range model.Measures {
				if measure.Aggregation == "count" && measure.Field == "" {
					continue
				}
				source := parent
				if measure.Source == "child" {
					source = child
				}
				field := source.GetField(measure.Field)
				if field == nil || (measure.Aggregation != "count" && !field.IsNumeric()) {
					return fmt.Errorf("report %q model %q measure %q references an invalid numeric field %q", report.Name, model.Name, measure.Name, measure.Field)
				}
			}
			for _, filter := range relation.Filters {
				source := parent
				if filter.Source == "child" {
					source = child
				}
				if source.GetField(filter.Field) == nil {
					return fmt.Errorf("report %q model %q fixed filter references unknown field %q", report.Name, model.Name, filter.Field)
				}
			}
		}
	}
	return nil
}
