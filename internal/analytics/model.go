package analytics

import "github.com/asenawritescode/kora/internal/doctype"

// DataPointKind is the generic analytical shape derived from a DocType.
// It is the stable model behind auto-generated metrics and page insights.
type DataPointKind string

const (
	DataPointCount             DataPointKind = "count"
	DataPointCreatedDaily      DataPointKind = "created_daily"
	DataPointCountByField      DataPointKind = "count_by_field"
	DataPointSum               DataPointKind = "sum"
	DataPointStateDistribution DataPointKind = "state_distribution"
)

// DataPointSpec describes one analytic fact family that can be tracked for a
// DocType. A single DocType can yield many point specs from its fields and
// workflow definition.
type DataPointSpec struct {
	Name      string        `json:"name" yaml:"name"`
	Label     string        `json:"label" yaml:"label"`
	Kind      DataPointKind `json:"kind" yaml:"kind"`
	DocType   string        `json:"doctype" yaml:"doctype"`
	Field     string        `json:"field,omitempty" yaml:"field,omitempty"`
	LinkField string        `json:"link_field,omitempty" yaml:"link_field,omitempty"`
	TimeField string        `json:"time_field,omitempty" yaml:"time_field,omitempty"`
	FromState string        `json:"from_state,omitempty" yaml:"from_state,omitempty"`
	ToState   string        `json:"to_state,omitempty" yaml:"to_state,omitempty"`
	Auto      bool          `json:"auto" yaml:"auto"`
}

// BuildDataPointCatalog returns the generic data points implied by a DocType.
func BuildDataPointCatalog(dt *doctype.DocType) []DataPointSpec {
	if dt == nil {
		return nil
	}
	out := []DataPointSpec{
		{
			Name:    metricName(dt.Name) + "_count",
			Label:   dt.Name + " Count",
			Kind:    DataPointCount,
			DocType: dt.Name,
			Auto:    true,
		},
		{
			Name:      metricName(dt.Name) + "_created_daily",
			Label:     dt.Name + " Created (Daily)",
			Kind:      DataPointCreatedDaily,
			DocType:   dt.Name,
			TimeField: "creation",
			Auto:      true,
		},
	}

	for _, f := range dt.Fields {
		if f.IsLayoutField() || f.Fieldtype == "Table" {
			continue
		}
		switch f.Fieldtype {
		case "Select", "Link", "Dynamic Link":
			out = append(out, DataPointSpec{
				Name:      metricName(dt.Name) + "_count_by_" + metricName(f.Fieldname),
				Label:     dt.Name + " by " + f.Label,
				Kind:      DataPointCountByField,
				DocType:   dt.Name,
				Field:     f.Fieldname,
				LinkField: f.Fieldname,
				Auto:      true,
			})
		case "Int", "Float", "Currency", "Percent":
			out = append(out, DataPointSpec{
				Name:    metricName(dt.Name) + "_sum_" + metricName(f.Fieldname),
				Label:   "Total " + f.Label,
				Kind:    DataPointSum,
				DocType: dt.Name,
				Field:   f.Fieldname,
				Auto:    true,
			})
		}
	}

	if dt.IsSubmittable {
		out = append(out, DataPointSpec{
			Name:    metricName(dt.Name) + "_state_distribution",
			Label:   dt.Name + " by State",
			Kind:    DataPointStateDistribution,
			DocType: dt.Name,
			Auto:    true,
		})
	}

	return out
}
