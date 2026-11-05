package query

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// ErrTooManyGroups is the code reported when an aggregate query yields more than limit groups.
const ErrTooManyGroups = "query.too_many_groups"

// ErrInputInvalid is the code reported (HTTP 400) for bad call input.
const ErrInputInvalid = "query.input_invalid"

// Request carries one call's verified context. Permission checks happen
// before the executor; it applies scope and owner_field row limits.
type Request struct {
	Input      map[string]any
	Actor      string
	Now        time.Time
	FullRead   bool   // role may read all rows: owner_field is not applied
	ScopeField string // DocType business-scope column; empty for tenant scope
	ScopeValue any    // verified BusinessScope value
	Cursor     []any  // sort-column values + name of the last row seen
}

// Page is one result page. Next is nil on the last page.
type Page struct {
	Rows []map[string]any
	Next []any
}

type builder struct {
	d    db.Dialect
	req  Request
	args []any
}

func (b *builder) bind(v any) string { b.args = append(b.args, v); return "?" }

func (b *builder) value(v any) (any, bool, error) {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "$") {
		return v, true, nil
	}
	switch {
	case s == "$actor":
		return b.req.Actor, true, nil
	case s == "$now":
		return b.req.Now.UTC().Format("2006-01-02 15:04:05"), true, nil
	case s == "$today":
		return b.req.Now.UTC().Format("2006-01-02"), true, nil
	case strings.HasPrefix(s, "$input."):
		in, ok := b.req.Input[strings.TrimPrefix(s, "$input.")]
		return in, ok && in != nil, nil
	}
	return nil, false, fmt.Errorf("%s: unknown value reference %q", ErrInputInvalid, s)
}

var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// leaf renders one comparison; ok=false means an optional input was absent.
func (b *builder) leaf(f Filter) (string, bool, error) {
	if s, ok := f.Value.(string); ok && strings.HasPrefix(s, "$field.") {
		ops := map[string]string{"eq": "=", "ne": "<>", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}
		op, ok := ops[f.Op]
		if !ok {
			return "", false, fmt.Errorf("%s: $field needs a comparison operator", ErrInputInvalid)
		}
		return b.d.QuoteIdent(f.Field) + " " + op + " " + b.d.QuoteIdent(strings.TrimPrefix(s, "$field.")), true, nil
	}
	v, present, err := b.value(f.Value)
	if err != nil {
		return "", false, err
	}
	col := b.d.QuoteIdent(f.Field)
	if f.Op == "is_null" {
		return col + " IS NULL", true, nil
	}
	if !present {
		if f.Optional {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%s: missing value for %s", ErrInputInvalid, f.Field)
	}
	switch f.Op {
	case "eq", "ne", "lt", "lte", "gt", "gte":
		ops := map[string]string{"eq": "=", "ne": "<>", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}
		return col + " " + ops[f.Op] + " " + b.bind(v), true, nil
	case "contains", "starts_with":
		p := likeEscaper.Replace(fmt.Sprint(v)) + "%"
		if f.Op == "contains" {
			p = "%" + p
		}
		return col + " LIKE " + b.bind(p) + " ESCAPE '!'", true, nil
	case "in":
		list, _ := v.([]any)
		if len(list) == 0 {
			return "", false, fmt.Errorf("%s: in needs a non-empty list for %s", ErrInputInvalid, f.Field)
		}
		ph := make([]string, len(list))
		for i, e := range list {
			ph[i] = b.bind(e)
		}
		return col + " IN (" + strings.Join(ph, ", ") + ")", true, nil
	}
	return "", false, fmt.Errorf("unknown operator %q", f.Op)
}

func (b *builder) filter(f Filter) (string, bool, error) {
	if len(f.Any) == 0 {
		return b.leaf(f)
	}
	var parts []string
	for _, c := range f.Any {
		s, ok, err := b.leaf(c)
		if err != nil {
			return "", false, err
		}
		if ok {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "", false, nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", true, nil
}

// keyset renders the "after cursor" predicate over sort columns + name.
func (b *builder) keyset(sorts []Sort) (string, error) {
	if len(b.req.Cursor) != len(sorts) {
		return "", fmt.Errorf("%s: cursor does not match sort", ErrInputInvalid)
	}
	var ors []string
	for i, s := range sorts {
		var and []string
		for j := 0; j < i; j++ {
			and = append(and, b.d.QuoteIdent(sorts[j].Field)+" = "+b.bind(b.req.Cursor[j]))
		}
		op := ">"
		if s.Dir == "desc" {
			op = "<"
		}
		and = append(and, b.d.QuoteIdent(s.Field)+" "+op+" "+b.bind(b.req.Cursor[i]))
		ors = append(ors, "("+strings.Join(and, " AND ")+")")
	}
	return "(" + strings.Join(ors, " OR ") + ")", nil
}

// Build returns the dialect-bound SQL and args for one page (limit+1 rows).
// The sort list always ends with name for a stable keyset; the last
// len(sort) selected columns are cursor values.
func Build(d *Definition, dialect db.Dialect, req Request) (string, []any, []Sort, error) {
	b := &builder{d: dialect, req: req}
	if len(d.Aggregates) > 0 {
		return buildAggregate(b, d)
	}
	sorts := append([]Sort{}, d.Sort...)
	sorts = append(sorts, Sort{Field: "name", Dir: "asc"})
	cols := make([]string, 0, len(d.Fields)+len(sorts))
	for _, f := range d.Fields {
		cols = append(cols, dialect.QuoteIdent(f))
	}
	for _, s := range sorts {
		cols = append(cols, dialect.QuoteIdent(s.Field))
	}
	var where []string
	for _, f := range d.Filters {
		s, ok, err := b.filter(f)
		if err != nil {
			return "", nil, nil, err
		}
		if ok {
			where = append(where, s)
		}
	}
	if d.OwnerField != "" && !req.FullRead {
		where = append(where, dialect.QuoteIdent(d.OwnerField)+" = "+b.bind(req.Actor))
	}
	if req.ScopeField != "" {
		where = append(where, dialect.QuoteIdent(req.ScopeField)+" = "+b.bind(req.ScopeValue))
	}
	if req.Cursor != nil {
		k, err := b.keyset(sorts)
		if err != nil {
			return "", nil, nil, err
		}
		where = append(where, k)
	}
	order := make([]string, len(sorts))
	for i, s := range sorts {
		order[i] = dialect.QuoteIdent(s.Field) + " " + strings.ToUpper(orDefault(s.Dir, "asc"))
	}
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + dialect.QuoteIdent(sourceTable(d))
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + strings.Join(order, ", ") + fmt.Sprintf(" LIMIT %d", d.Limit+1)
	return db.Rebind(dialect, q), b.args, sorts, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Run executes the query and returns one page. Values are returned as the
// driver's exact text for byte-typed columns (decimals stay exact).
func Run(ctx context.Context, conn *sql.DB, dialect db.Dialect, d *Definition, req Request) (*Page, error) {
	q, args, sorts, err := Build(d, dialect, req)
	if err != nil {
		return nil, err
	}
	if len(d.Aggregates) > 0 {
		return runAggregate(ctx, conn, d, q, args)
	}
	rs, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", d.Name, err)
	}
	defer rs.Close()
	n := len(d.Fields) + len(sorts)
	page := &Page{}
	var lastCursor []any
	for rs.Next() {
		vals := make([]any, n)
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("query %s: %w", d.Name, err)
		}
		for i, v := range vals {
			if bs, ok := v.([]byte); ok {
				vals[i] = string(bs)
			}
		}
		if len(page.Rows) == d.Limit {
			page.Next = lastCursor
			break
		}
		row := make(map[string]any, len(d.Fields))
		for i, f := range d.Fields {
			row[f] = vals[i]
		}
		page.Rows = append(page.Rows, row)
		lastCursor = vals[len(d.Fields):]
	}
	return page, rs.Err()
}

// buildAggregate renders the grouped SELECT; filters, owner and scope apply before GROUP BY.
func buildAggregate(b *builder, d *Definition) (string, []any, []Sort, error) {
	if b.req.Cursor != nil {
		return "", nil, nil, fmt.Errorf("%s: aggregate queries take no cursor", ErrInputInvalid)
	}
	q := b.d
	var cols, group []string
	for _, g := range d.GroupBy {
		cols = append(cols, q.QuoteIdent(g))
		group = append(group, q.QuoteIdent(g))
	}
	for _, a := range d.Aggregates {
		if a.Fn == "count" {
			cols = append(cols, "COUNT(*)")
		} else {
			cols = append(cols, "COALESCE(SUM("+q.QuoteIdent(a.Field)+"), 0)")
		}
	}
	var where []string
	for _, f := range d.Filters {
		s, ok, err := b.filter(f)
		if err != nil {
			return "", nil, nil, err
		}
		if ok {
			where = append(where, s)
		}
	}
	if d.OwnerField != "" && !b.req.FullRead {
		where = append(where, q.QuoteIdent(d.OwnerField)+" = "+b.bind(b.req.Actor))
	}
	if b.req.ScopeField != "" {
		where = append(where, q.QuoteIdent(b.req.ScopeField)+" = "+b.bind(b.req.ScopeValue))
	}
	sql := "SELECT " + strings.Join(cols, ", ") + " FROM " + q.QuoteIdent(sourceTable(d))
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sorts := d.Sort
	if len(sorts) == 0 {
		for _, g := range d.GroupBy {
			sorts = append(sorts, Sort{Field: g, Dir: "asc"})
		}
	}
	if len(group) > 0 {
		order := make([]string, len(sorts))
		for i, s := range sorts {
			order[i] = q.QuoteIdent(s.Field) + " " + strings.ToUpper(orDefault(s.Dir, "asc"))
		}
		sql += " GROUP BY " + strings.Join(group, ", ") + " ORDER BY " + strings.Join(order, ", ")
	}
	sql += fmt.Sprintf(" LIMIT %d", d.Limit+1)
	return db.Rebind(q, sql), b.args, sorts, nil
}

func runAggregate(ctx context.Context, conn *sql.DB, d *Definition, q string, args []any) (*Page, error) {
	rs, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", d.Name, err)
	}
	defer rs.Close()
	n := len(d.Fields)
	page := &Page{}
	for rs.Next() {
		vals := make([]any, n)
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("query %s: %w", d.Name, err)
		}
		if len(page.Rows) == d.Limit {
			return nil, fmt.Errorf("%s: query %s has more than %d groups", ErrTooManyGroups, d.Name, d.Limit)
		}
		row := make(map[string]any, n)
		for i, f := range d.Fields {
			row[f] = aggValue(d, i, vals[i])
		}
		page.Rows = append(page.Rows, row)
	}
	return page, rs.Err()
}

// aggValue keeps count as an integer and sums as exact decimal text.
func aggValue(d *Definition, i int, v any) any {
	k := i - len(d.GroupBy)
	if k < 0 {
		if bs, ok := v.([]byte); ok {
			return string(bs)
		}
		return v
	}
	if d.Aggregates[k].Fn == "count" {
		return v
	}
	switch x := v.(type) {
	case []byte:
		return string(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// sourceTable maps the Source DocType name to its physical table name.
func sourceTable(d *Definition) string {
	return (&doctype.DocType{Name: d.Source}).RawTableName()
}
