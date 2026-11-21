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
	name  string           // as requested: "supplier" or "product.supplier.town"
	alias string           // column of the derived table the group reads
	joins []join           // one per Link field followed; empty for a fact dimension
	leaf  string           // field read from the last joined record type; empty for a fact dimension
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

// dimensionOf resolves one requested dimension of the planned model. index is its position in the
// query, which keeps the aliases of different paths apart.
func (p *plan) dimensionOf(cat Catalog, name string, index int) (dimension, error) {
	if !strings.Contains(name, ".") {
		return p.factDimension(cat, name)
	}
	return p.pathDimension(cat, name, index)
}

// factDimension resolves a fact dimension. A Link dimension is labelled with the titles of the record
// type it names, when the caller may read that record type.
func (p *plan) factDimension(cat Catalog, name string) (dimension, error) {
	if !isDimension(p.model.Facts, name) {
		return dimension{}, fmt.Errorf("dimension %q is not a facts dimension of %s", name, p.model.Name)
	}
	dim := dimension{name: name, alias: name}
	if fld := p.factField(cat, name); fld != nil && fld.Fieldtype == "Link" {
		dim.label = p.labelTarget(cat, fld)
	}
	return dim, nil
}

// factField is the record field a fact dimension reads: a header field, or a field of the child table
// named by the facts lines. It is nil when the field cannot be found.
func (p *plan) factField(cat Catalog, column string) *doctype.Field {
	f := p.model.Facts
	for _, d := range f.Dimensions {
		if d.ColumnName() != column {
			continue
		}
		if !fieldPath(d.Field).isLine() {
			return p.model.GetField(column)
		}
		lf := p.model.GetField(f.Lines)
		if lf == nil || cat.Models[lf.Options] == nil {
			return nil
		}
		return cat.Models[lf.Options].GetField(column)
	}
	return nil
}

// pathDimension resolves "a.b.c": a header Link dimension a, then Link field b on the record type a
// names, then field c on the last record type. Every record type joined to must be joinable.
func (p *plan) pathDimension(cat Catalog, name string, index int) (dimension, error) {
	parts := strings.Split(name, ".")
	links, leafName := parts[:len(parts)-1], parts[len(parts)-1]
	switch {
	case len(links) > maxHops:
		return dimension{}, fmt.Errorf("dimension %q follows %d links: a path follows at most %d", name, len(links), maxHops)
	case !headerDimension(p.model.Facts, links[0]):
		return dimension{}, fmt.Errorf("dimension %q must start at a header dimension of %s", name, p.model.Name)
	}
	dim := dimension{name: name, alias: fmt.Sprintf("_path%d", index)}
	cur, prev := p.model, factAlias
	for i, link := range links {
		fld := cur.GetField(link)
		if fld == nil || fld.Fieldtype != "Link" {
			return dimension{}, fmt.Errorf("dimension %q: %q is not a Link field of %s", name, link, cur.Name)
		}
		target, err := p.joinTo(cat, name, fld)
		if err != nil {
			return dimension{}, err
		}
		j := join{alias: fmt.Sprintf("_d%d_%d", index, i), prev: prev, field: link, model: target}
		dim.joins = append(dim.joins, j)
		cur, prev = target, j.alias
	}
	return p.leafOf(cat, dim, cur, leafName)
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
