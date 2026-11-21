package metrics

import (
	"cmp"
	"fmt"
	"slices"
)

// Closure lists the named metrics and every metric they are computed from, sorted by name. A name
// the catalog does not define is an error.
func (c Catalog) Closure(names []string) ([]Metric, error) {
	seen := map[string]bool{}
	var out []Metric
	var visit func(name string) error
	visit = func(name string) error {
		if seen[name] {
			return nil
		}
		m, ok := c.Metrics[name]
		if !ok {
			return fmt.Errorf("metric %q is not defined", name)
		}
		seen[name] = true
		out = append(out, m)
		for _, ref := range m.refs() {
			if err := visit(ref); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b Metric) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Models lists the record types the metrics read, once each and sorted. Derived and ratio metrics
// read through their components, so only fact metrics contribute.
func Models(ms []Metric) []string {
	var out []string
	for _, m := range ms {
		if m.Model != "" && !slices.Contains(out, m.Model) {
			out = append(out, m.Model)
		}
	}
	slices.Sort(out)
	return out
}

// Check compiles q against the catalog without running it, so a caller can refuse a bad query
// before it is authorized or sent to the database.
func (c Catalog) Check(q Query) error {
	_, err := planQuery(c, q)
	return err
}
