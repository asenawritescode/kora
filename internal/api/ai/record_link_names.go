package ai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// linkNames maps a Link field name to the display name of each code it holds. A code without a name is absent.
type linkNames map[string]map[string]string

// show returns the code with its name as "Name (CODE)", or the code alone when no name is known or the name
// equals the code.
func (n linkNames) show(field, code string) string {
	name := n[field][code]
	if name == "" || name == code {
		return code
	}
	return fmt.Sprintf("%s (%s)", name, code)
}

// fieldTitles returns the name of each Link field of doc that has one, keyed by field name.
func (n linkNames) fieldTitles(fields []doctype.Field, doc *doctype.Document) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		code, ok := doc.Fields[f.Fieldname].(string)
		if !ok {
			continue
		}
		if name := n[f.Fieldname][code]; name != "" && name != code {
			out[f.Fieldname] = name
		}
	}
	return out
}

// linkCodes returns, per target record type, the distinct non-empty codes that the Link fields of docs hold,
// in first-seen order. A Link without a target is skipped. It does no I/O.
func linkCodes(fields []doctype.Field, docs []*doctype.Document) map[string][]string {
	out := map[string][]string{}
	seen := map[string]map[string]bool{}
	for _, f := range fields {
		if f.Fieldtype != "Link" || f.Options == "" {
			continue
		}
		for _, doc := range docs {
			if code, ok := linkValue(doc, f.Fieldname); ok {
				addLinkCode(out, seen, f.Options, code)
			}
		}
	}
	return out
}

// linkValue returns the code a Link field of doc holds. A missing document, an empty value or a value that is
// not text reports false.
func linkValue(doc *doctype.Document, fieldname string) (string, bool) {
	if doc == nil {
		return "", false
	}
	code, ok := doc.Fields[fieldname].(string)
	return code, ok && strings.TrimSpace(code) != ""
}

// addLinkCode appends code to out[target] unless it is already there. seen records the codes added per target.
func addLinkCode(out map[string][]string, seen map[string]map[string]bool, target, code string) {
	if seen[target] == nil {
		seen[target] = map[string]bool{}
	}
	if seen[target][code] {
		return
	}
	seen[target][code] = true
	out[target] = append(out[target], code)
}

// readableLinkFields returns the readable Link fields of dt.
func readableLinkFields(dt *doctype.DocType) []doctype.Field {
	var fields []doctype.Field
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Link" && aiReadableField(f) {
			fields = append(fields, f)
		}
	}
	return fields
}

// readLinkNames returns the names of the codes that the readable Link fields of dt hold in docs. A target record
// type gives names only when the person may read every one of its rows, because the title lookup applies no row
// scope. Each target type is looked up once. A failed lookup is logged and keeps the codes.
func readLinkNames(ctx context.Context, tx *orm.TxManager, dt *doctype.DocType, docs []*doctype.Document, execution policy.Execution) linkNames {
	names := linkNames{}
	if tx == nil || tx.Registry == nil || dt == nil {
		return names
	}
	fields := readableLinkFields(dt)
	for target, codes := range linkCodes(fields, docs) {
		rec := recordTypeNamed(tx.Registry, target)
		if rec == nil || !unrestrictedRead(tx, rec, execution) {
			continue
		}
		titles, err := tx.ResolveTitles(ctx, rec, codes)
		if err != nil {
			slog.WarnContext(ctx, "ai link names unavailable, keeping codes", "record_type", dt.Name, "target", target, "error", err)
			continue
		}
		names.addTitles(fields, target, titles)
	}
	return names
}

// addTitles records the titles of one target type under every Link field that points at it.
func (n linkNames) addTitles(fields []doctype.Field, target string, titles map[string]string) {
	for _, f := range fields {
		if f.Options != target {
			continue
		}
		if n[f.Fieldname] == nil {
			n[f.Fieldname] = map[string]string{}
		}
		for code, title := range titles {
			if title != "" {
				n[f.Fieldname][code] = title
			}
		}
	}
}

// unrestrictedRead reports whether the person may read every row of rec: the read is allowed with no owner-only
// or assigned-field restriction, and the type is not legal-entity owned.
func unrestrictedRead(tx *orm.TxManager, rec *doctype.DocType, execution policy.Execution) bool {
	if rec.BusinessScopePolicy == contract.ScopeEntityOwned {
		return false
	}
	scope, denied := authorizeRecordOperation(tx, tx.Registry, rec, "read", execution)
	return denied == "" && !scope.Owner && len(scope.AssignedFields) == 0
}
