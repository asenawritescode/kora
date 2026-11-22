package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/titles"
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
		var (
			titles map[string]string
			err    error
		)
		switch {
		case dim.user:
			titles, err = e.userNames(ctx, codesOf(agg, keys, i))
		case dim.label != nil:
			titles, err = e.titles(ctx, dim.label, codesOf(agg, keys, i))
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i] = titles
	}
	return out, nil
}

// labelled reports whether any dimension is labelled: it names a record type whose titles label it, or it groups
// by user logins.
func (p plan) labelled() bool {
	for _, dim := range p.dims {
		if dim.label != nil || dim.user {
			return true
		}
	}
	return false
}

// userNames reads the name of each user login of the site: the full name, else the email, else the login. A login
// with no user has no name.
func (e Engine) userNames(ctx context.Context, logins []string) (map[string]string, error) {
	out := make(map[string]string, len(logins))
	for start := 0; start < len(logins); start += labelChunk {
		end := min(start+labelChunk, len(logins))
		chunk := logins[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, e.Site)
		for _, login := range chunk {
			args = append(args, login)
		}
		stmt := fmt.Sprintf("SELECT name, full_name, email FROM _kora_user WHERE site = ? AND name IN (%s)",
			strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", "))
		rows, err := e.DB.QueryContext(ctx, db.Rebind(e.Dialect, stmt), args...)
		if err != nil {
			return nil, fmt.Errorf("metrics: read user names: %w", err)
		}
		if err := scanUserNames(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanUserNames reads the rows of a user-name lookup into out and closes them.
func scanUserNames(rows *sql.Rows, out map[string]string) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var login string
		var fullName, email sql.NullString
		if err := rows.Scan(&login, &fullName, &email); err != nil {
			return fmt.Errorf("metrics: scan user names: %w", err)
		}
		switch {
		case fullName.String != "":
			out[login] = fullName.String
		case email.String != "":
			out[login] = email.String
		default:
			out[login] = login
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("metrics: read user names: %w", err)
	}
	return nil
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

// titles reads the display title of each code from its record type, following Link titles through the catalog
// (see titles.Resolve). A code with no record has no title.
func (e Engine) titles(ctx context.Context, model *doctype.DocType, codes []string) (map[string]string, error) {
	return titles.Resolve(ctx, titles.Options{DB: e.DB, Dialect: e.Dialect, Link: e.linkTarget}, model, codes)
}

// linkTarget is the record type a Link title names, when its titles may be read; the catalog holds every type.
func (e Engine) linkTarget(name string) *doctype.DocType {
	if t := e.Catalog.Models[name]; titles.Readable(t) {
		return t
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
