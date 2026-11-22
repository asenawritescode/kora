package orm

import (
	"context"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/titles"
)

// ResolveTitles returns the display title of each record name of dt, following Link titles (see titles.Resolve).
// It reads within the transaction the manager is bound to, and follows a Link only to a record type that is readable.
func (tx *TxManager) ResolveTitles(ctx context.Context, dt *doctype.DocType, names []string) (map[string]string, error) {
	opts := titles.Options{DB: tx.queryer(), Dialect: tx.Dialect, Link: tx.linkTarget}
	return titles.Resolve(ctx, opts, dt, names)
}

// linkTarget is the readable record type a Link title names, or nil.
func (tx *TxManager) linkTarget(name string) *doctype.DocType {
	if tx.Registry == nil {
		return nil
	}
	if t := tx.Registry.Get(name); titles.Readable(t) {
		return t
	}
	return nil
}
