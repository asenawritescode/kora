package metrics

import (
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// maxHops is the most Link fields one dimension path may follow. The first link is a header fact
// dimension, so "product.supplier.town" follows two.
const maxHops = 2

// factAlias is the alias of the fact table in the fact-part statement. Joined record types can share
// column names with the fact table, so every fact column is qualified with it.
const factAlias = "t"

// dimension is one grouping column of a plan. A fact dimension reads its own column of the derived
// table. A path follows joins from the fact column through Link fields and reads leaf from the last
// record type it reaches; alias names that column of the derived table.
type dimension struct {
	name  string           // as requested: "supplier", "items.product" or "items.product.category"
	alias string           // column of the derived table the group reads
	line  bool             // the fact dimension is on the child table named by facts lines
	joins []join           // one per Link field followed; empty for a plain fact dimension
	leaf  string           // field read from the last joined record type; empty for a plain fact dimension
	label *doctype.DocType // record type whose titles label the group values; nil for none
}

// join is a LEFT JOIN from prev, through its Link column field, to the linked record type. It matches
// on the record name, which is unique, so it never repeats a fact row.
type join struct {
	alias string           // table alias of the linked record type
	prev  string           // alias of the table that holds field: the fact table, or the previous join
	field string           // the Link column on prev
	model *doctype.DocType // the linked record type
}

// lastAlias is the alias of the record type a path ends on.
func (d dimension) lastAlias() string {
	return d.joins[len(d.joins)-1].alias
}

// factDimOf finds the fact dimension a requested name reads, and how many leading name segments name it.
// "category" is a header dimension (one segment); "items.product" is a dimension of the child table named by
// facts lines (two segments). The segments after those are a path through Link fields.
func factDimOf(f *doctype.FactModel, name string) (doctype.FactDimension, int, bool) {
	parts := strings.Split(name, ".")
	if f.Lines != "" && len(parts) >= 2 && parts[0] == f.Lines {
		for _, d := range f.Dimensions {
			if fieldPath(d.Field).isLine() && d.ColumnName() == parts[1] {
				return d, 2, true
			}
		}
		return doctype.FactDimension{}, 0, false
	}
	for _, d := range f.Dimensions {
		if d.ColumnName() == parts[0] {
			return d, 1, true
		}
	}
	if f.GroupsByStatus(parts[0]) {
		return doctype.FactDimension{Field: doctype.StatusColumn}, 1, true
	}
	return doctype.FactDimension{}, 0, false
}

// dimensionOf resolves one requested dimension of the planned model. index is its position in the
// query, which keeps the aliases of different paths apart.
func (p *plan) dimensionOf(cat Catalog, name string, index int) (dimension, error) {
	fd, segments, ok := factDimOf(p.model.Facts, name)
	if !ok {
		return dimension{}, fmt.Errorf("dimension %q is not a facts dimension of %s", name, p.model.Name)
	}
	dim := dimension{name: name, alias: fd.ColumnName(), line: fieldPath(fd.Field).isLine()}
	rest := strings.Split(name, ".")[segments:]
	if len(rest) == 0 {
		if holder := p.holderOf(cat, fd); holder != nil {
			if fld := holder.GetField(fd.ColumnName()); fld != nil && fld.Fieldtype == "Link" {
				dim.label = p.labelTarget(cat, fld)
			}
		}
		return dim, nil
	}
	return p.pathDimension(cat, dim, fd, rest, index)
}

// holderOf is the record type that holds a fact dimension's field: the record, or the child table for a
// dimension named by facts lines. It is nil when the child table is not in the catalog.
func (p *plan) holderOf(cat Catalog, fd doctype.FactDimension) *doctype.DocType {
	if !fieldPath(fd.Field).isLine() {
		return p.model
	}
	lf := p.model.GetField(p.model.Facts.Lines)
	if lf == nil {
		return nil
	}
	return cat.Models[lf.Options]
}

// pathDimension resolves a dimension whose fact field fd is followed by rest: Link fields, then a leaf.
// The fact field is the first link, so a path follows len(rest) links: "items.product.category" follows
// one (product) and reads category from it.
func (p *plan) pathDimension(cat Catalog, dim dimension, fd doctype.FactDimension, rest []string, index int) (dimension, error) {
	if len(rest) > maxHops {
		return dimension{}, fmt.Errorf("dimension %q follows %d links: a path follows at most %d", dim.name, len(rest), maxHops)
	}
	holder := p.holderOf(cat, fd)
	if holder == nil {
		return dimension{}, fmt.Errorf("dimension %q: lines table %q is not defined", dim.name, p.model.Facts.Lines)
	}
	dim.alias = fmt.Sprintf("_path%d", index)
	links := append([]string{fd.ColumnName()}, rest[:len(rest)-1]...)
	cur, prev := holder, factAlias
	for i, link := range links {
		fld := cur.GetField(link)
		if fld == nil || fld.Fieldtype != "Link" {
			return dimension{}, fmt.Errorf("dimension %q: %q is not a Link field of %s", dim.name, link, cur.Name)
		}
		target, err := p.joinTo(cat, dim.name, fld)
		if err != nil {
			return dimension{}, err
		}
		j := join{alias: fmt.Sprintf("_d%d_%d", index, i), prev: prev, field: link, model: target}
		dim.joins = append(dim.joins, j)
		cur, prev = target, j.alias
	}
	return p.leafOf(cat, dim, cur, rest[len(rest)-1])
}

// joinTo is the record type a Link field names. It is refused with ErrForbidden when the caller may
// not join to it.
func (p *plan) joinTo(cat Catalog, name string, fld *doctype.Field) (*doctype.DocType, error) {
	target := cat.Models[fld.Options]
	if target == nil {
		return nil, fmt.Errorf("dimension %q: link %q names no record type", name, fld.Fieldname)
	}
	if !p.scope.Joinable[target.Name] {
		return nil, fmt.Errorf("dimension %q: %w", name, ErrForbidden)
	}
	return target, nil
}

// leafOf sets the field a path reads on its last record type. A Link leaf is labelled like a Link
// fact dimension.
func (p *plan) leafOf(cat Catalog, dim dimension, last *doctype.DocType, leafName string) (dimension, error) {
	fld := last.GetField(leafName)
	if fld == nil || !fieldKind(fld.Fieldtype).groupable() {
		return dimension{}, fmt.Errorf("dimension %q: %q is not a dimension field of %s", dim.name, leafName, last.Name)
	}
	dim.leaf = leafName
	if fld.Fieldtype == "Link" {
		dim.label = p.labelTarget(cat, fld)
	}
	return dim, nil
}

// labelTarget is the record type a Link field names when the caller may read it, otherwise nil. A
// group value is then shown as its code, without a title.
func (p *plan) labelTarget(cat Catalog, fld *doctype.Field) *doctype.DocType {
	target := cat.Models[fld.Options]
	if target == nil || !p.scope.Joinable[target.Name] {
		return nil
	}
	return target
}

// fieldKind is the Fieldtype of a record field.
type fieldKind string

// groupable reports whether a field of this kind can group a query: the kinds a facts dimension takes.
func (k fieldKind) groupable() bool {
	switch k {
	case "Link", "Select", "Data", "Check":
		return true
	}
	return false
}

// joinClauses renders the LEFT JOINs of every path dimension, in dimension order.
func (p plan) joinClauses(d db.Dialect) string {
	var b strings.Builder
	for _, dim := range p.dims {
		for _, j := range dim.joins {
			fmt.Fprintf(&b, " LEFT JOIN %s AS %s ON %s.%s = %s.%s",
				d.QuoteIdent(j.model.RawTableName()), j.alias, j.alias, d.QuoteIdent("name"), j.prev, d.QuoteIdent(j.field))
		}
	}
	return b.String()
}

// hasLineDim reports whether a dimension is on the child table. Such a query reads the line rows only:
// a header row has no line to group by.
func (p plan) hasLineDim() bool {
	for _, dim := range p.dims {
		if dim.line {
			return true
		}
	}
	return false
}

// hasJoins reports whether any dimension is a path, which only the facts route can answer.
func (p plan) hasJoins() bool {
	for _, dim := range p.dims {
		if len(dim.joins) > 0 {
			return true
		}
	}
	return false
}

// dimNames lists the requested dimensions by name, in request order.
func (p plan) dimNames() []string {
	names := make([]string, len(p.dims))
	for i, dim := range p.dims {
		names[i] = dim.name
	}
	return names
}
