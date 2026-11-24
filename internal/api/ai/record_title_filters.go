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
// on the codes. The search is matched the same way on the record's own title. A filter that cannot be
// resolved is kept as the person wrote it, so the validator reports it.
func resolveTitleFilters(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, in titleInput) (titleResolution, error) {
	filters, search, rowScope, execution := in.filters, in.search, in.rowScope, in.execution
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
	if search == "" {
		return out, nil
	}
	matches, err := tx.MatchTitles(ctx, dt, search, rowScope)
	if err != nil {
		return titleResolution{}, err
	}
	if matches.Kind == "" {
		out.none, err = noMatchingRowsText(ctx, tx, dt, rowScope)
		return out, err
	}
	out.filters = append(out.filters, map[string]any{"field": "name", "op": "in", "value": codesOf(matches.Matches)})
	return out, nil
}

// resolveLinkFilter matches the values of one Link filter by title. It returns the filter unchanged when it is
// not a Link filter, its target is unknown or unreadable, or its value is not text.
func resolveLinkFilter(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, filter map[string]any, execution policy.Execution) (any, []string, string, error) {
	field, _ := filter["field"].(string)
	op := strings.ToLower(fmt.Sprint(filter["op"]))
	f := dt.GetField(field)
	if f == nil || f.Fieldtype != "Link" || (op != "=" && op != "like" && op != "in") || tx.Registry == nil {
		return filter, nil, "", nil
	}
	target := recordTypeNamed(tx.Registry, f.Options)
	if target == nil {
		return filter, nil, "", nil
	}
	targetScope, denied := authorizeRecordOperation(tx, tx.Registry, target, "read", execution)
	if denied != "" {
		return filter, nil, "", nil
	}
	values := linkValues(op, filter["value"])
	if len(values) == 0 {
		return filter, nil, "", nil
	}
	var codes []any
	var notes []string
	for _, value := range values {
		text := strings.Trim(value, "%")
		matches, err := tx.MatchTitles(ctx, target, text, targetScope)
		if err != nil {
			return nil, nil, "", err
		}
		if matches.Kind == "" {
			return nil, nil, noTargetMatchText(target.Name, text, matches.Closest), nil
		}
		codes = append(codes, codesOf(matches.Matches)...)
		if matches.Kind != orm.TitleKindCode {
			notes = append(notes, matchNote(field, text, target.Name, matches.Matches))
		}
	}
	return map[string]any{"field": field, "op": "in", "value": codes}, notes, "", nil
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
	out := fmt.Sprintf("No %s matches %q.", recordType, text)
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
	return fmt.Sprintf("No %s match these filters. %s %s exist in total.", dt.Name, total, dt.Name), nil
}

// noRowsText is the reply of find_records when no row is returned: a plain none when nothing was asked, else
// the filter reply with the record type's total.
func noRowsText(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, rowScope policy.RowScope, resolved titleResolution) string {
	if len(resolved.filters) == 0 {
		return fmt.Sprintf("No %s found.", dt.Name)
	}
	text, err := noMatchingRowsText(ctx, tx, dt, rowScope)
	if err != nil {
		return aiToolError{Code: errGuardFailed, Message: "Could not search " + dt.Name + "."}.text()
	}
	return strings.Join(append(resolved.notes, text), "\n")
}
