package metrics

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

// AdHoc is one measure a question asks for without a saved metric: a count, a sum of a fact measure, or a
// count_distinct of a fact dimension, optionally filtered.
type AdHoc struct {
	Model   string
	Agg     string
	Field   string
	Filters []Filter
}

// ExploreCatalog lists the record types that have a fact model and that read allows, sorted by name.
// Each offers a count, a sum for every header measure and a count_distinct for every header dimension.
// Child-table measures and dimensions are left out. A Link dimension also offers the fields of the record
// type it names, and the fields of a record type linked from that one, up to maxHops links. Only record
// types join allows are followed. docs holds every record type, so a Link can name any of them.
func ExploreCatalog(docs []*doctype.DocType, read, join map[string]bool) []contract.ExploreRecordType {
	links := exploreLinks{models: make(map[string]*doctype.DocType, len(docs)), join: join}
	for _, dt := range docs {
		links.models[dt.Name] = dt
	}
	var out []contract.ExploreRecordType
	for _, dt := range docs {
		if dt.Facts == nil || dt.IsChildTable || !read[dt.Name] {
			continue
		}
		out = append(out, exploreRecord(dt, links))
	}
	slices.SortFunc(out, func(a, b contract.ExploreRecordType) int { return strings.Compare(a.RecordType, b.RecordType) })
	return out
}

// exploreLinks resolves the Link fields of the catalog's record types.
type exploreLinks struct {
	models map[string]*doctype.DocType
	join   map[string]bool
}

// target is the record type a Link field names when joins to it are allowed, otherwise nil.
func (l exploreLinks) target(fld *doctype.Field) *doctype.DocType {
	dt := l.models[fld.Options]
	if dt == nil || !l.join[dt.Name] {
		return nil
	}
	return dt
}

// exploreRecord describes one record type with a fact model.
func exploreRecord(dt *doctype.DocType, links exploreLinks) contract.ExploreRecordType {
	f := dt.Facts
	rec := contract.ExploreRecordType{RecordType: dt.Name, Label: dt.Name, Description: dt.Description, TimeField: f.Time,
		Measures: []contract.ExploreMeasure{{Key: "count", Agg: aggCount, Label: "Records", Format: "number"}}}
	for _, m := range f.Measures {
		col := m.ColumnName()
		if strings.Contains(m.Field, ".") {
			continue
		}
		fld := dt.GetField(col)
		rec.Measures = append(rec.Measures, contract.ExploreMeasure{Key: "sum:" + col, Agg: aggSum, Field: col,
			Label: fieldLabel(dt, col), Format: formatOf(fld)})
	}
	for _, d := range f.Dimensions {
		if fieldPath(d.Field).isLine() {
			rec.Dimensions = append(rec.Dimensions, lineDimensions(dt, d.ColumnName(), links)...)
			continue
		}
		col := d.ColumnName()
		fld := dt.GetField(col)
		rec.Dimensions = append(rec.Dimensions, contract.ExploreDimension{Path: col, Label: fieldLabel(dt, col), Kind: kindOf(fld)})
		rec.Measures = append(rec.Measures, contract.ExploreMeasure{Key: "distinct:" + col, Agg: aggCountDistinct, Field: col,
			Label: "Distinct " + fieldLabel(dt, col), Format: "number"})
		rec.Dimensions = append(rec.Dimensions, linkedDimensions(dt, col, col, links)...)
	}
	return rec
}

// lineDimensions lists a dimension of the child table named by facts lines, as lines.<col>, and the paths
// through it. The child table's own fields are the source of its label and its links.
func lineDimensions(dt *doctype.DocType, col string, links exploreLinks) []contract.ExploreDimension {
	lf := dt.GetField(dt.Facts.Lines)
	if lf == nil || links.models[lf.Options] == nil {
		return nil
	}
	lines := links.models[lf.Options]
	fld := lines.GetField(col)
	if fld == nil {
		return nil
	}
	path := dt.Facts.Lines + "." + col
	out := []contract.ExploreDimension{{Path: path, Label: fieldLabel(lines, col), Kind: kindOf(fld)}}
	return append(out, linkedDimensions(lines, col, path, links)...)
}

// linkedDimensions lists the paths a Link dimension opens: each dimension field of the record type it names
// (one hop), and each dimension field of a record type that one of its Link fields names (two hops). src
// holds the field col, and path is the name the dimension goes by. Nothing is listed when joins to the
// record type are not allowed.
func linkedDimensions(src *doctype.DocType, col, path string, links exploreLinks) []contract.ExploreDimension {
	fld := src.GetField(col)
	if fld == nil || fld.Fieldtype != "Link" {
		return nil
	}
	first := links.target(fld)
	if first == nil {
		return nil
	}
	head := fieldLabel(src, col) + " ›"
	out := pathFields(path, first, head, 1)
	for i := range first.Fields {
		mid := &first.Fields[i]
		if mid.Fieldtype != "Link" {
			continue
		}
		second := links.target(mid)
		if second == nil {
			continue
		}
		out = append(out, pathFields(path+"."+mid.Fieldname, second, head+" "+fieldLabel(first, mid.Fieldname)+" ›", 2)...)
	}
	return out
}

// pathFields lists the dimension fields of rec, each as the path prefix followed by the field name.
func pathFields(prefix string, rec *doctype.DocType, group string, hops int) []contract.ExploreDimension {
	var out []contract.ExploreDimension
	for _, f := range rec.Fields {
		if !fieldKind(f.Fieldtype).groupable() {
			continue
		}
		out = append(out, contract.ExploreDimension{Path: prefix + "." + f.Fieldname, Label: fieldLabel(rec, f.Fieldname),
			Kind: f.Fieldtype, Hops: hops, Group: group})
	}
	return out
}

// fieldLabel is the field's label, or its column name when the record type does not declare the field.
func fieldLabel(dt *doctype.DocType, column string) string {
	if fld := dt.GetField(column); fld != nil && fld.Label != "" {
		return fld.Label
	}
	return column
}

// formatOf is how a field's numbers display: currency for Currency, otherwise a plain number.
func formatOf(fld *doctype.Field) string {
	if fld != nil && fld.Fieldtype == "Currency" {
		return "currency"
	}
	return "number"
}

// kindOf is the field type a dimension came from, or Data when the field is not declared.
func kindOf(fld *doctype.Field) string {
	if fld == nil {
		return "Data"
	}
	return fld.Fieldtype
}

// WithAdHoc returns the catalog with one unverified simple metric per measure and the metric names in
// request order. A metric is named adhoc_<model>_<agg>_<field>, labelled "<measure> of <record type>" with
// the filters appended, and validated like a declared metric. A repeated measure is one metric; a name a
// declared metric holds gets a numeric suffix.
func (c Catalog) WithAdHoc(measures []AdHoc) (Catalog, []string, error) {
	if len(measures) == 0 {
		return c, nil, nil
	}
	out := Catalog{Metrics: maps.Clone(c.Metrics), Models: c.Models}
	var names []string
	for _, a := range measures {
		m, err := c.adHocMetric(a)
		if err != nil {
			return Catalog{}, nil, err
		}
		if err := m.validate(catalog{models: out.Models, metrics: out.Metrics}); err != nil {
			return Catalog{}, nil, fmt.Errorf("measure %s on %q: %w", a.Agg, a.Model, err)
		}
		if sameAdHoc(out, names, m) {
			continue
		}
		m.Name = freeName(out.Metrics, m.Name)
		out.Metrics[m.Name] = m
		names = append(names, m.Name)
	}
	return out, names, nil
}

// sameAdHoc reports whether an unsaved measure already in the query computes what m computes.
func sameAdHoc(c Catalog, names []string, m Metric) bool {
	for _, n := range names {
		have := c.Metrics[n]
		if have.Model == m.Model && reflect.DeepEqual(have.Measure, m.Measure) && reflect.DeepEqual(have.Filters, m.Filters) {
			return true
		}
	}
	return false
}

// freeName is name, or name with the first numeric suffix that no metric holds.
func freeName(taken map[string]Metric, name string) string {
	if _, ok := taken[name]; !ok {
		return name
	}
	for i := 2; ; i++ {
		if candidate := fmt.Sprintf("%s_%d", name, i); !hasMetric(taken, candidate) {
			return candidate
		}
	}
}

// hasMetric reports whether taken holds name.
func hasMetric(taken map[string]Metric, name string) bool {
	_, ok := taken[name]
	return ok
}

// adHocMetric builds the unvalidated metric of one measure.
func (c Catalog) adHocMetric(a AdHoc) (Metric, error) {
	dt := c.Models[a.Model]
	if dt == nil {
		return Metric{}, fmt.Errorf("record type %q is not defined", a.Model)
	}
	label := "Records"
	if a.Agg != aggCount {
		label = fieldLabel(dt, a.Field)
	}
	if a.Agg == aggCountDistinct {
		label = "Distinct " + label
	}
	label += " of " + a.Model
	for i, f := range a.Filters {
		if i == 0 {
			label += " where"
		} else {
			label += " and"
		}
		label += fmt.Sprintf(" %s %s %v", f.Field, f.Op, f.Value)
	}
	name := "adhoc_" + slug(a.Model) + "_" + a.Agg
	if a.Field != "" {
		name += "_" + slug(a.Field)
	}
	return Metric{Name: name, Label: label, Type: KindSimple, Model: a.Model,
		Measure: &Measure{Agg: a.Agg, Field: a.Field}, Filters: a.Filters}, nil
}

// slug lowercases a name and turns every run of other characters into one underscore.
func slug(s string) string {
	var b strings.Builder
	under := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			under = false
			continue
		}
		if !under {
			b.WriteByte('_')
		}
		under = true
	}
	return strings.Trim(b.String(), "_")
}
