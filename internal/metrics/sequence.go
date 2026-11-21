package metrics

import (
	"context"
	"fmt"

	"github.com/asenawritescode/kora/internal/doctype"
)

// SourceSequence is the newest source sequence the fact tables of models have applied: the largest
// source_sequence on any header or line row. Zero when the tables are empty. A query reads the summaries,
// which lag the fact rows, so this is the freshness a result can claim at most.
func (e Engine) SourceSequence(ctx context.Context, models []*doctype.DocType) (int64, error) {
	var newest int64
	for _, dt := range models {
		if dt == nil || dt.Facts == nil {
			continue
		}
		tables := []string{dt.Facts.TableName(dt)}
		if dt.Facts.Lines != "" {
			tables = append(tables, dt.Facts.LineTableName(dt))
		}
		for _, table := range tables {
			var seq int64
			stmt := fmt.Sprintf("SELECT COALESCE(MAX(%s), 0) FROM %s", e.Dialect.QuoteIdent("source_sequence"), e.Dialect.QuoteIdent(table)) //nolint:gosec // identifiers only, quoted by the dialect
			if err := e.DB.QueryRowContext(ctx, stmt).Scan(&seq); err != nil {
				return 0, fmt.Errorf("metrics: source sequence of %s: %w", table, err)
			}
			newest = max(newest, seq)
		}
	}
	return newest, nil
}
