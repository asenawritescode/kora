package metrics

import (
	"context"
	"slices"
)

// Currencies lists the distinct currency values the rows of q carry, for each record type q reads that
// declares a currency field. A value is the empty string for a row in the base currency. Each record type
// is read through its own simple metrics over q's range, filters and scope, so the values are the ones the
// query sums. It returns nil when q reads no record type with a currency field.
func (e Engine) Currencies(ctx context.Context, q Query) ([]string, error) {
	ms, err := e.Catalog.Closure(q.Metrics)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, model := range Models(ms) {
		dt := e.Catalog.Models[model]
		if dt == nil || dt.Facts == nil || dt.Facts.CurrencyField == "" {
			continue
		}
		values, err := e.modelCurrencies(ctx, q, ms, model, dt.Facts.CurrencyField)
		if err != nil {
			return nil, err
		}
		out = append(out, values...)
	}
	if out == nil {
		return nil, nil
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// modelCurrencies runs the closure's metrics of one record type grouped by its currency field and returns
// the group values the result holds.
func (e Engine) modelCurrencies(ctx context.Context, q Query, ms []Metric, model, field string) ([]string, error) {
	var names []string
	for _, m := range ms {
		if m.Model == model {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	res, err := e.Query(ctx, Query{Metrics: names, Dimensions: []string{field}, Filters: q.Filters,
		From: q.From, To: q.To, Grain: GrainRange, Scope: q.Scope})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range res.Rows {
		if len(row.Group) > 0 {
			out = append(out, row.Group[0])
		}
	}
	return out, nil
}
