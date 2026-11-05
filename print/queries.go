package print

import (
	"regexp"
	"strings"
)

// MaxQueries caps the queries: entries of one format.
const MaxQueries = 4

var queryKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// QuerySpec runs one published query: Query is "<owner module>/<name>" and
// Input values are doc.<field>, receipt.<key> or a literal string.
type QuerySpec struct {
	Query string            `yaml:"query" json:"query"`
	Input map[string]string `yaml:"input,omitempty" json:"input,omitempty"`
}

// Ref splits Query into the owner module and query name.
func (q QuerySpec) Ref() (module, name string, ok bool) {
	module, name, ok = strings.Cut(q.Query, "/")
	return module, name, ok && module != "" && name != "" && !strings.Contains(name, "/")
}

func validateQueries(qs map[string]QuerySpec) error {
	if len(qs) > MaxQueries {
		return invalid("at most %d queries", MaxQueries)
	}
	for k, q := range qs {
		if !queryKey.MatchString(k) {
			return invalid("query key %q must be a lower-case identifier", k)
		}
		if _, _, ok := q.Ref(); !ok {
			return invalid("query %q: query must be <module>/<name>", k)
		}
		for in, v := range q.Input {
			if in == "" || v == "doc." || v == "receipt." || v == "doc" || v == "receipt" {
				return invalid("query %q: bad input ref %q", k, v)
			}
		}
	}
	return nil
}

// ResolveInput evaluates a spec's input against the loaded doc and receipt.
func (q QuerySpec) ResolveInput(doc, receipt map[string]any) map[string]any {
	out := make(map[string]any, len(q.Input))
	for k, v := range q.Input {
		switch {
		case strings.HasPrefix(v, "doc."):
			out[k] = doc[strings.TrimPrefix(v, "doc.")]
		case strings.HasPrefix(v, "receipt."):
			out[k] = receipt[strings.TrimPrefix(v, "receipt.")]
		default:
			out[k] = v
		}
	}
	return out
}
