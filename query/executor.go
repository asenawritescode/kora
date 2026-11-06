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

// InputError is a call the caller got wrong: a missing required input, an
// empty "in" list, a cursor that does not match the sort. Callers report it
// as a validation failure, never as a server fault.
type InputError struct {
	Field string
	Msg   string
}

func (e *InputError) Error() string { return ErrInputInvalid + ": " + e.Msg }

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
	Limit      int    // page size the caller asked for; 0 means the definition's limit
}

// pageSize is the rows to return: the caller's limit, bounded by the
// definition's limit (the published page_limit).
func (r Request) pageSize(d *Definition) (int, error) {
	switch {
	case r.Limit == 0:
		return d.Limit, nil
	case r.Limit < 0 || r.Limit > d.Limit:
		return 0, &InputError{Field: "limit", Msg: fmt.Sprintf("limit must be between 1 and %d for this query", d.Limit)}
	}
	return r.Limit, nil
}

// Page is one result page. Next is nil on the last page.
type Page struct {
	Rows []map[string]any
	Next []any
}

type builder struct {
	d      db.Dialect
	inputs map[string]InputSpec
	via    bool
	req    Request
	args   []any
}

// col renders a column reference; child sources qualify every column with
// the child alias c, and parent.<field> with the parent alias p.
func (b *builder) col(name string) string {
	if !b.via {
		return b.d.QuoteIdent(name)
	}
	if pf, ok := strings.CutPrefix(name, "parent."); ok {
		return b.d.QuoteIdent("p") + "." + b.d.QuoteIdent(pf)
	}
	return b.d.QuoteIdent("c") + "." + b.d.QuoteIdent(name)
}

// scopeCol renders the legal-entity column, which lives on the parent for child sources.
func (b *builder) scopeCol() string {
	if b.via {
		return b.col("parent." + b.req.ScopeField)
	}
	return b.d.QuoteIdent(b.req.ScopeField)
}

// from renders the FROM clause: the source table, or child JOIN parent for via sources.
func (b *builder) from(d *Definition) string {
	if d.Via == "" {
		return b.d.QuoteIdent(sourceTable(d))
	}
	parent := (&doctype.DocType{Name: d.ParentDocType()}).RawTableName()
	_, field, _ := strings.Cut(d.Via, ".")
	return b.d.QuoteIdent(parent+"__"+field) + " " + b.d.QuoteIdent("c") + " JOIN " + b.d.QuoteIdent(parent) + " " + b.d.QuoteIdent("p") + " ON " + b.col("parent") + " = " + b.col("parent.name")
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
		name := strings.TrimPrefix(s, "$input.")
		if in, ok := b.req.Input[name]; ok && in != nil {
			// The catalog publishes a reference default as its token, so a client
			// echoing it back means "use the default", not that literal string.
			if s, isStr := in.(string); isStr && s == b.inputs[name].Default && isReference(s) {
				return b.value(s)
			}
			return in, true, nil
		}
		if def := b.inputs[name].Default; def != nil {
			return b.value(def)
		}
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("%s: unknown value reference %q", ErrInputInvalid, s)
}

// isReference reports whether s is a server-resolved default token.
func isReference(s string) bool { return s == "$today" || s == "$now" || s == "$actor" }

var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// leaf renders one comparison; ok=false means an optional input was absent.
func (b *builder) leaf(f Filter) (string, bool, error) {
	if s, ok := f.Value.(string); ok && strings.HasPrefix(s, "$field.") {
		ops := map[string]string{"eq": "=", "ne": "<>", "lt": "<", "lte": "<=", "gt": ">", "gte": ">="}
		op, ok := ops[f.Op]
		if !ok {
			return "", false, fmt.Errorf("%s: $field needs a comparison operator", ErrInputInvalid)
		}
		return b.col(f.Field) + " " + op + " " + b.col(strings.TrimPrefix(s, "$field.")), true, nil
	}
	v, present, err := b.value(f.Value)
	if err != nil {
		return "", false, err
	}
	col := b.col(f.Field)
	if f.Op == "is_null" {
		return col + " IS NULL", true, nil
	}
	if !present {
		if f.Optional {
			return "", false, nil
		}
		name, _ := f.Value.(string)
		name = strings.TrimPrefix(name, "$input.")
		return "", false, &InputError{Field: name, Msg: "input " + name + " is required"}
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
			return "", false, &InputError{Field: f.Field, Msg: "in needs a non-empty list for " + f.Field}
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
		return "", &InputError{Field: "cursor", Msg: "cursor does not match sort"}
	}
	var ors []string
	for i, s := range sorts {
		var and []string
		for j := 0; j < i; j++ {
			and = append(and, b.col(sorts[j].Field)+" = "+b.bind(b.req.Cursor[j]))
		}
		op := ">"
		if s.Dir == "desc" {
			op = "<"
		}
		and = append(and, b.col(s.Field)+" "+op+" "+b.bind(b.req.Cursor[i]))
		ors = append(ors, "("+strings.Join(and, " AND ")+")")
	}
	return "(" + strings.Join(ors, " OR ") + ")", nil
}

// Build returns the dialect-bound SQL and args for one page (limit+1 rows).
// The sort list always ends with name for a stable keyset; the last
// len(sort) selected columns are cursor values.
func Build(d *Definition, dialect db.Dialect, req Request) (string, []any, []Sort, error) {
	b := &builder{d: dialect, inputs: d.Input, req: req, via: d.Via != ""}
	size, err := req.pageSize(d)
	if err != nil {
		return "", nil, nil, err
	}
	if len(d.Aggregates) > 0 {
		return buildAggregate(b, d)
	}
	sorts := append([]Sort{}, d.Sort...)
	sorts = append(sorts, Sort{Field: "name", Dir: "asc"})
	cols := make([]string, 0, len(d.Fields)+len(sorts))
	for _, f := range d.Fields {
		cols = append(cols, b.col(f))
	}
	for _, s := range sorts {
		cols = append(cols, b.col(s.Field))
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
		where = append(where, b.scopeCol()+" = "+b.bind(req.ScopeValue))
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
		order[i] = b.col(s.Field) + " " + strings.ToUpper(orDefault(s.Dir, "asc"))
	}
	q := "SELECT " + strings.Join(cols, ", ") + " FROM " + b.from(d)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + strings.Join(order, ", ") + fmt.Sprintf(" LIMIT %d", size+1)
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
		return runAggregate(ctx, conn, d, q, args, req.Limit)
	}
	size, err := req.pageSize(d)
	if err != nil {
		return nil, err
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
		if len(page.Rows) == size {
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
		return "", nil, nil, &InputError{Field: "cursor", Msg: "aggregate queries take no cursor"}
	}
	q := b.d
	var cols, group []string
	for _, g := range d.GroupBy {
		cols = append(cols, b.col(g))
		group = append(group, b.col(g))
	}
	for _, a := range d.Aggregates {
		if a.Fn == "count" {
			cols = append(cols, "COUNT(*)")
		} else {
			cols = append(cols, "COALESCE(SUM("+b.col(a.Field)+"), 0)")
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
		where = append(where, b.scopeCol()+" = "+b.bind(b.req.ScopeValue))
	}
	sql := "SELECT " + strings.Join(cols, ", ") + " FROM " + b.from(d)
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
			order[i] = b.col(s.Field) + " " + strings.ToUpper(orDefault(s.Dir, "asc"))
		}
		sql += " GROUP BY " + strings.Join(group, ", ") + " ORDER BY " + strings.Join(order, ", ")
	}
	sql += fmt.Sprintf(" LIMIT %d", d.Limit+1)
	return db.Rebind(q, sql), b.args, sorts, nil
}

// runAggregate cannot page, so a caller limit below the group count is
// refused instead of truncating the answer.
func runAggregate(ctx context.Context, conn *sql.DB, d *Definition, q string, args []any, limit int) (*Page, error) {
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
		if limit > 0 && len(page.Rows) > limit {
			return nil, &InputError{Field: "limit", Msg: fmt.Sprintf("query %s has more than %d groups and cannot be paged: raise limit", d.Name, limit)}
		}
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
