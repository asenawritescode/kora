package ai

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// maxCountedRows is the most rows a no-match total counts. Past it the reply says "more than" it.
const maxCountedRows = 1000

// titleResolution is what matching names and titles adds to a find: the filters to run, one note per name the
// person typed that matched, and the reply when a link value or the search names no record ("" when none).
type titleResolution struct {
	filters []any
	notes   []string
	none    string
}

// titleInput is what find_records asks to resolve: the filters the person sent, the search text, and the
// person's read scope and execution for the record type and each Link target.
type titleInput struct {
	filters   []any
	search    string
	rowScope  policy.RowScope
	execution policy.Execution
}

// resolveTitleFilters turns names typed in find_records into record codes. A Link filter on = , like or in
// whose target the person may read has each value matched by title (orm.MatchTitles) and becomes an in filter
// on the codes. The search is matched the same way on the record's own title, then on the Link fields named in
// the search fields (see resolveSearch). A filter that cannot be
// resolved is kept as the person wrote it, so the validator reports it.
func resolveTitleFilters(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, in titleInput) (titleResolution, error) {
	filters, execution := in.filters, in.execution
	out := titleResolution{filters: make([]any, 0, len(filters)+1)}
	for _, raw := range filters {
		filter, ok := raw.(map[string]any)
		if !ok {
			out.filters = append(out.filters, raw)
			continue
		}
		resolved, notes, none, err := resolveLinkFilter(ctx, tx, dt, filter, execution)
		if err != nil {
			return titleResolution{}, err
		}
		out.filters = append(out.filters, resolved)
		out.notes = append(out.notes, notes...)
		if none != "" {
			out.none = none
			return out, nil
		}
	}
	if in.search == "" {
		return out, nil
	}
	return resolveSearch(ctx, tx, dt, in, out)
}

// resolveSearch matches the search on the record's own title first. When no title matches, it tries each Link
// field named in the search fields of dt, and the first whose target matches the search becomes the filter. When
// neither matches, the reply is the honest zero.
func resolveSearch(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, in titleInput, out titleResolution) (titleResolution, error) {
	matches, err := tx.MatchTitles(ctx, dt, in.search, in.rowScope)
	if err != nil {
		return titleResolution{}, err
	}
	if matches.Kind != "" {
		out.filters = append(out.filters, map[string]any{"field": "name", "op": "in", "value": codesOf(matches.Matches)})
		return out, nil
	}
	linked, notes, err := searchThroughLinks(ctx, tx, dt, in)
	if err != nil {
		return titleResolution{}, err
	}
	if linked != nil {
		out.filters = append(out.filters, linked)
		out.notes = append(out.notes, notes...)
		return out, nil
	}
	out.none, err = noMatchingRowsText(ctx, tx, dt, in.rowScope)
	return out, err
}

// searchThroughLinks matches the search by the title of each readable Link field named in the search fields of
// dt. The first Link whose target matches wins: it returns an in filter on the codes and the notes for them. It
// returns a nil filter when no Link matches.
func searchThroughLinks(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, in titleInput) (map[string]any, []string, error) {
	fields := linkSearchFields(dt)
	for i := range fields {
		lt, ok := readableLinkTarget(tx, &fields[i], in.execution)
		if !ok {
			continue
		}
		m, err := matchLinkValues(ctx, tx, lt, []string{in.search})
		if err != nil {
			return nil, nil, err
		}
		if m.none == "" && len(m.codes) > 0 {
			return map[string]any{"field": lt.field, "op": "in", "value": m.codes}, m.notes, nil
		}
	}
	return nil, nil, nil
}

// linkSearchFields returns the Link fields of dt that its search fields name, in declaration order, each once.
// Names that are not fields of dt, are not Links or are hidden from the assistant are skipped.
func linkSearchFields(dt *doctype.DocType) []doctype.Field {
	named := map[string]bool{}
	for _, name := range strings.Split(dt.SearchFields, ",") {
		named[strings.TrimSpace(name)] = true
	}
	var out []doctype.Field
	for _, f := range dt.Fields {
		if f.Fieldtype == "Link" && named[f.Fieldname] && aiReadableField(f) {
			out = append(out, f)
		}
	}
	return out
}

// linkTarget is a Link field of the record type, the target record type it names and the rows the person may
// read there.
type linkTarget struct {
	field string
	doc   *doctype.DocType
	scope policy.RowScope
}

// linkMatch is what matching typed names against one Link target gives: the codes, one note per name that
// matched by title, and the reply when a name matches nothing ("" when every name matched).
type linkMatch struct {
	codes []any
	notes []string
	none  string
}

// resolveLinkFilter matches the values of one Link filter by title. It returns the filter unchanged when it is
// not a Link filter, its target is unknown or unreadable, or its value is not text.
func resolveLinkFilter(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, filter map[string]any, execution policy.Execution) (any, []string, string, error) {
	field, _ := filter["field"].(string)
	op := strings.ToLower(fmt.Sprint(filter["op"]))
	f := dt.GetField(field)
	if f == nil || (op != "=" && op != "like" && op != "in") {
		return filter, nil, "", nil
	}
	lt, ok := readableLinkTarget(tx, f, execution)
	if !ok {
		return filter, nil, "", nil
	}
	values := linkValues(op, filter["value"])
	if len(values) == 0 {
		return filter, nil, "", nil
	}
	m, err := matchLinkValues(ctx, tx, lt, values)
	if err != nil {
		return nil, nil, "", err
	}
	if m.none != "" {
		return nil, nil, m.none, nil
	}
	return map[string]any{"field": field, "op": "in", "value": m.codes}, m.notes, "", nil
}

// readableLinkTarget returns the target of a Link field, with the rows the person may read there. It reports
// false when the field is not a Link, its target is unknown or the person may not read the target.
func readableLinkTarget(tx *orm.TxManager, f *doctype.Field, execution policy.Execution) (linkTarget, bool) {
	if f.Fieldtype != "Link" || tx.Registry == nil {
		return linkTarget{}, false
	}
	target := recordTypeNamed(tx.Registry, f.Options)
	if target == nil {
		return linkTarget{}, false
	}
	scope, denied := authorizeRecordOperation(tx, tx.Registry, target, "read", execution)
	if denied != "" {
		return linkTarget{}, false
	}
	return linkTarget{field: f.Fieldname, doc: target, scope: scope}, true
}

// matchLinkValues matches each typed text by title in the Link target. A text that matches no record stops the
// match and sets the reply that says so.
func matchLinkValues(ctx context.Context, tx *orm.TxManager, lt linkTarget, texts []string) (linkMatch, error) {
	var out linkMatch
	for _, value := range texts {
		text := strings.Trim(value, "%")
		matches, err := tx.MatchTitles(ctx, lt.doc, text, lt.scope)
		if err != nil {
			return linkMatch{}, err
		}
		if matches.Kind == "" {
			return linkMatch{none: noTargetMatchText(lt.doc.Name, text, matches.Closest)}, nil
		}
		out.codes = append(out.codes, codesOf(matches.Matches)...)
		if matches.Kind != orm.TitleKindCode {
			out.notes = append(out.notes, matchNote(lt.field, text, lt.doc.Name, matches.Matches))
		}
	}
	return out, nil
}

// linkValues returns the text values of a Link filter's value, or nil when it is not text.
func linkValues(op string, value any) []string {
	if op != "in" {
		text, ok := value.(string)
		if !ok {
			return nil
		}
		return []string{text}
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, text)
	}
	return out
}

// codesOf returns the record names of matches, in order.
func codesOf(matches []orm.TitleMatch) []any {
	codes := make([]any, 0, len(matches))
	for _, m := range matches {
		codes = append(codes, m.Name)
	}
	return codes
}

// matchNote says which record a typed name resolved to, for one Link value.
func matchNote(field, text, recordType string, matches []orm.TitleMatch) string {
	if len(matches) == 1 {
		return fmt.Sprintf("Matched %s %q → %s (%s).", field, text, matches[0].Title, matches[0].Name)
	}
	parts := make([]string, 0, len(matches))
	for _, m := range matches {
		parts = append(parts, fmt.Sprintf("%s (%s)", m.Title, m.Name))
	}
	return fmt.Sprintf("Matched %s %q → %d %ss: %s.", field, text, len(matches), strings.ToLower(recordType), strings.Join(parts, ", "))
}

// noTargetMatchText says that a Link value names no record, with the nearest titles when there are any.
func noTargetMatchText(recordType, text string, closest []orm.TitleMatch) string {
	out := fmt.Sprintf("No %s%s%q.", recordType, noTargetMatch, text)
	if len(closest) == 0 {
		return out
	}
	titles := make([]string, 0, len(closest))
	for _, c := range closest {
		titles = append(titles, c.Title)
	}
	return out + " Closest: " + strings.Join(titles, ", ") + "."
}

// noMatchingRowsText is the reply when the filters leave no row, with the record type's total.
func noMatchingRowsText(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, rowScope policy.RowScope) (string, error) {
	n, err := tx.CountUpTo(ctx, dt, rowScope, maxCountedRows+1)
	if err != nil {
		return "", err
	}
	total := strconv.Itoa(n)
	if n > maxCountedRows {
		total = fmt.Sprintf("more than %d", maxCountedRows)
	}
	return fmt.Sprintf("No %s%s%s %s exist in total.", dt.Name, noFilterMatch, total, dt.Name), nil
}

// The sentence endings of the zero-row replies. The writers in this file build them with these constants and
// findReply.empty reads them back, so the two cannot drift apart.
const (
	noneFoundEnding = " found."                // "No <type> found."
	noFilterMatch   = " match these filters. " // "No <type> match these filters. ..."
	noTargetMatch   = " matches "              // `No <type> matches "<text>".`
)

// noRowsText is the reply of find_records when no row is returned: a plain none when nothing was asked, else
// the filter reply with the record type's total.
func noRowsText(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, rowScope policy.RowScope, resolved titleResolution) string {
	if len(resolved.filters) == 0 {
		return "No " + dt.Name + noneFoundEnding
	}
	text, err := noMatchingRowsText(ctx, tx, dt, rowScope)
	if err != nil {
		return aiToolError{Code: errGuardFailed, Message: "Could not search " + dt.Name + "."}.text()
	}
	return strings.Join(append(resolved.notes, text), "\n")
}

// findReply is the text a find_records call returns.
type findReply string

// empty says that the reply is one of the zero-row replies above, on any of its lines.
func (r findReply) empty() bool {
	for _, line := range strings.Split(string(r), "\n") {
		if !strings.HasPrefix(line, "No ") {
			continue
		}
		if strings.HasSuffix(line, noneFoundEnding) || strings.Contains(line, noFilterMatch) || strings.Contains(line, noTargetMatch) {
			return true
		}
	}
	return false
}
