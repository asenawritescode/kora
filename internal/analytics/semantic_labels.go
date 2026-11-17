package analytics

import (
	"database/sql"
	"fmt"
	"strings"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// applyLinkLabels replaces Link ids with the linked record's label field; an id
// without a label keeps its stored value.
func (qe *QueryEngine) applyLinkLabels(call semanticCall, rows []map[string]any) error {
	if call.model.Relation != nil || len(call.query.Dimensions) != 1 {
		return nil
	}
	name := call.query.Dimensions[0]
	var dimension SemanticDimension
	for _, candidate := range call.model.Dimensions {
		if candidate.Name == name {
			dimension = candidate
		}
	}
	if dimension.LinkDoctype == "" {
		return nil
	}
	if call.registry == nil || call.registry.Get(dimension.LinkDoctype) == nil {
		return fmt.Errorf("linked analytics DocType %q is unavailable", dimension.LinkDoctype)
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = strings.TrimPrefix(fmt.Sprint(row[name]), name+"=")
	}
	labels, err := qe.linkedLabels(call.registry.Get(dimension.LinkDoctype), dimension.LinkLabelField, ids)
	if err != nil {
		return err
	}
	for i, row := range rows {
		row[name] = ids[i]
		// The raw key drives drill-down; "<dimension>.key" cannot clash with a field name.
		row[name+".key"] = ids[i]
		if label, found := labels[ids[i]]; found {
			row[name] = label
		}
	}
	return nil
}

// linkedLabels loads the label field of the given ids in one linked DocType.
func (qe *QueryEngine) linkedLabels(linked *doctype.DocType, labelField string, ids []string) (map[string]string, error) {
	labels := map[string]string{}
	if len(ids) == 0 {
		return labels, nil
	}
	q, args := qe.Dialect.QuoteIdent, make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	statement := "SELECT " + q("name") + ", " + q(labelField) + " FROM " + q(linked.RawTableName()) + " WHERE " + q("name") + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	rows, err := qe.DB.Query(kdb.Rebind(qe.Dialect, statement), args...)
	if err != nil {
		return nil, fmt.Errorf("load link labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var label sql.NullString
		if err := rows.Scan(&id, &label); err != nil {
			return nil, fmt.Errorf("scan link label: %w", err)
		}
		if label.Valid && label.String != "" {
			labels[id] = label.String
		}
	}
	return labels, rows.Err()
}
