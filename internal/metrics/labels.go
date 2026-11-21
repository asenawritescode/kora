package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// labelChunk bounds the codes one title statement looks up, so a wide result stays under the
// driver's placeholder limit.
const labelChunk = 500

// labelsOf returns, for each dimension, the titles of the records its group values name, or nil when
// no dimension is labelled. Each labelled dimension is one batched lookup, not one per row.
func (e Engine) labelsOf(ctx context.Context, p plan, agg aggregate, keys []string) ([]map[string]string, error) {
	if !p.labelled() {
		return nil, nil
	}
	out := make([]map[string]string, len(p.dims))
	for i, dim := range p.dims {
		if dim.label == nil {
			continue
		}
		titles, err := e.titles(ctx, dim.label, codesOf(agg, keys, i))
		if err != nil {
			return nil, err
		}
		out[i] = titles
	}
	return out, nil
}

// labelled reports whether any dimension names a record type whose titles label it.
func (p plan) labelled() bool {
	for _, dim := range p.dims {
		if dim.label != nil {
			return true
		}
	}
	return false
}

// codesOf lists the distinct non-empty values dimension i takes across keys, in key order. The Other
// group names no record and is skipped.
func codesOf(agg aggregate, keys []string, i int) []string {
	seen := map[string]bool{}
	var codes []string
	for _, k := range keys {
		if k == otherGroup {
			continue
		}
		if v := agg.groups[k][i]; v != "" && !seen[v] {
			seen[v] = true
			codes = append(codes, v)
		}
	}
	return codes
}

// titles reads the title of each code from its record type: the title field, or the name when the
// record type declares no other. A code with no record has no title.
func (e Engine) titles(ctx context.Context, model *doctype.DocType, codes []string) (map[string]string, error) {
	title := "name"
	if f := model.GetField(model.TitleField); f != nil {
		title = f.Fieldname
	}
	out := make(map[string]string, len(codes))
	for start := 0; start < len(codes); start += labelChunk {
		end := min(start+labelChunk, len(codes))
		if err := e.titleChunk(ctx, model, title, codes[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// titleChunk reads the titles of one chunk of codes into out.
func (e Engine) titleChunk(ctx context.Context, model *doctype.DocType, title string, codes []string, out map[string]string) error {
	q := e.Dialect.QuoteIdent
	args := make([]any, len(codes))
	for i, c := range codes {
		args[i] = c
	}
	stmt := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s IN (%s)", q("name"), q(title), q(model.RawTableName()), q("name"),
		strings.TrimSuffix(strings.Repeat("?, ", len(codes)), ", "))
	rows, err := e.DB.QueryContext(ctx, db.Rebind(e.Dialect, stmt), args...)
	if err != nil {
		return fmt.Errorf("metrics: read titles of %s: %w", model.Name, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var code string
		var val sql.NullString
		if err := rows.Scan(&code, &val); err != nil {
			return fmt.Errorf("metrics: scan titles of %s: %w", model.Name, err)
		}
		out[code] = val.String
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("metrics: read titles of %s: %w", model.Name, err)
	}
	return nil
}

// groupLabels returns the label of each dimension of one group: the title of its record, or empty.
func groupLabels(labels []map[string]string, group []string) []string {
	out := make([]string, len(group))
	for i, v := range group {
		out[i] = labels[i][v]
	}
	return out
}
