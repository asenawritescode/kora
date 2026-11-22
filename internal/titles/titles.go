// Package titles reads the display title of records. A record's title is its title_field; when that field is a
// Link, the title is the linked record's title, followed through at most MaxLinkHops Link fields.
package titles

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// MaxLinkHops is how many Link fields a title may be followed through. A Till Session titles through its
// Shift to the Shift's Register: two hops.
const MaxLinkHops = 2

// chunkSize bounds the codes one statement looks up, so a wide page stays under the driver's placeholder limit.
const chunkSize = 500

// Querier runs the read statements of a lookup. A *sql.DB, a *sql.Tx and the ORM's transaction queryer satisfy it.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Options is what a resolution reads with. Link returns the record type a Link field names, or nil when the
// caller may not read its titles; a nil Link follows no links.
type Options struct {
	DB      Querier
	Dialect db.Dialect
	Link    func(name string) *doctype.DocType
}

// Readable reports whether the titles of records of dt may be shown through a link: the record type is
// published (not a private module resource), not entity-owned, and its title field is neither hidden nor a password.
func Readable(dt *doctype.DocType) bool {
	if dt == nil {
		return false
	}
	title := strings.TrimSpace(dt.TitleField)
	if f := dt.GetField(title); title != "" && title != "name" && (f == nil || f.Hidden || strings.EqualFold(f.Fieldtype, "Password")) {
		return false
	}
	private := dt.ModuleOwnerID != "" && dt.ModuleAccess != contract.ModulePublished
	return !private && dt.BusinessScopePolicy != contract.ScopeEntityOwned
}

// Resolve returns the display title of each name of dt. A name whose record has no title, or is missing, is
// absent from the result. A Link title is followed through at most MaxLinkHops records; past that, or when the
// link cannot be read, the title is the raw value of the field.
// Each record type on the chain is read with one statement per chunk of names, never one per row.
func Resolve(ctx context.Context, o Options, dt *doctype.DocType, names []string) (map[string]string, error) {
	return resolve(ctx, o, dt, names, 0)
}

// resolve reads the title field of dt for names, then follows its Link while hops allows.
func resolve(ctx context.Context, o Options, dt *doctype.DocType, names []string, hops int) (map[string]string, error) {
	if dt == nil || len(names) == 0 {
		return map[string]string{}, nil
	}
	field := titleField(dt)
	out, err := readField(ctx, o, dt, field, names)
	if err != nil {
		return nil, err
	}
	if hops >= MaxLinkHops {
		return out, nil
	}
	target := linkTarget(o, dt, field)
	if target == nil {
		return out, nil
	}
	codes := distinct(out)
	if len(codes) == 0 {
		return out, nil
	}
	linked, err := resolve(ctx, o, target, codes, hops+1)
	if err != nil {
		return nil, err
	}
	for name, code := range out {
		if title := linked[code]; title != "" {
			out[name] = title
		}
	}
	return out, nil
}

// titleField is the field that titles dt: its title_field, or the record name when it declares none.
func titleField(dt *doctype.DocType) string {
	if f := dt.GetField(strings.TrimSpace(dt.TitleField)); f != nil {
		return f.Fieldname
	}
	return "name"
}

// linkTarget is the record type the field links to when the field is a Link the caller may read, otherwise nil.
func linkTarget(o Options, dt *doctype.DocType, field string) *doctype.DocType {
	f := dt.GetField(field)
	if f == nil || f.Fieldtype != "Link" || o.Link == nil {
		return nil
	}
	target := o.Link(strings.TrimSpace(f.Options))
	if target == nil || !Readable(target) {
		return nil
	}
	return target
}

// readField reads the value of field for each name of dt, chunked.
func readField(ctx context.Context, o Options, dt *doctype.DocType, field string, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for start := 0; start < len(names); start += chunkSize {
		end := min(start+chunkSize, len(names))
		chunk, err := readChunk(ctx, o, dt, field, names[start:end])
		if err != nil {
			return nil, err
		}
		maps.Copy(out, chunk)
	}
	return out, nil
}

// readChunk reads the values of field for one chunk of names. A record with no value gets an empty entry.
func readChunk(ctx context.Context, o Options, dt *doctype.DocType, field string, names []string) (map[string]string, error) {
	q := o.Dialect.QuoteIdent
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	stmt := fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s IN (%s)", q("name"), q(field), q(dt.RawTableName()), q("name"),
		strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", "))
	rows, err := o.DB.QueryContext(ctx, db.Rebind(o.Dialect, stmt), args...)
	if err != nil {
		return nil, fmt.Errorf("titles: read %s: %w", dt.Name, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string, len(names))
	for rows.Next() {
		var name string
		var val sql.NullString
		if err := rows.Scan(&name, &val); err != nil {
			return nil, fmt.Errorf("titles: scan %s: %w", dt.Name, err)
		}
		out[name] = val.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("titles: read %s: %w", dt.Name, err)
	}
	return out, nil
}

// distinct lists the distinct non-empty values of m, in no particular order.
func distinct(m map[string]string) []string {
	seen := make(map[string]bool, len(m))
	var out []string
	for _, v := range m {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
